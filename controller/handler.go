package controller

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	azureKeyVaultSecretv1alpha1 "github.com/SparebankenVest/azure-keyvault-controller/pkg/apis/azurekeyvaultcontroller/v1alpha1"
	clientset "github.com/SparebankenVest/azure-keyvault-controller/pkg/client/clientset/versioned"
	informers "github.com/SparebankenVest/azure-keyvault-controller/pkg/client/informers/externalversions/azurekeyvaultcontroller/v1alpha1"
	listers "github.com/SparebankenVest/azure-keyvault-controller/pkg/client/listers/azurekeyvaultcontroller/v1alpha1"
	"github.com/SparebankenVest/azure-keyvault-controller/vault"
)

// Handler process work on workqueues
type Handler struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// azureKeyvaultClientset is a clientset for our own API group
	azureKeyvaultClientset clientset.Interface

	secretsLister              corelisters.SecretLister
	azureKeyVaultSecretsLister listers.AzureKeyVaultSecretLister

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue      workqueue.RateLimitingInterface
	workqueueAzure workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

//NewHandler returns a new Handler
func NewHandler(kubeclientset kubernetes.Interface, azureKeyvaultClientset clientset.Interface, secretInformer coreinformers.SecretInformer, azureKeyVaultSecretsInformer informers.AzureKeyVaultSecretInformer, azureFrequency AzurePollFrequency) *Handler {
	return &Handler{}
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the AzureKeyVaultSecret resource
// with the current status of the resource.
func (h *Handler) syncHandler(key string) error {
	var azureKeyVaultSecret *azureKeyVaultSecretv1alpha1.AzureKeyVaultSecret
	var secret *corev1.Secret
	var err error

	// log.Infof("Checking state for %s", key)

	if azureKeyVaultSecret, err = h.getAzureKeyVaultSecret(key); err != nil {
		if exit := handleKeyVaultError(err, key); exit {
			return nil
		}
		return err
	}

	if secret, err = h.getOrCreateKubernetesSecret(azureKeyVaultSecret); err != nil {
		return err
	}

	if !metav1.IsControlledBy(secret, azureKeyVaultSecret) { // checks if the object has a controllerRef set to the given owner
		msg := fmt.Sprintf(MessageResourceExists, secret.Name)
		log.Warning(msg)
		h.recorder.Event(azureKeyVaultSecret, corev1.EventTypeWarning, ErrResourceExists, msg)
		return fmt.Errorf(msg)
	}

	// log.Info(MessageResourceSynced)
	h.recorder.Event(azureKeyVaultSecret, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	return nil
}

func (h *Handler) azureSyncHandler(key string) error {
	var azureKeyVaultSecret *azureKeyVaultSecretv1alpha1.AzureKeyVaultSecret
	var secret *corev1.Secret
	var secretValue string
	var err error

	log.Debugf("Checking state for %s in Azure", key)
	if azureKeyVaultSecret, err = h.getAzureKeyVaultSecret(key); err != nil {
		if exit := handleKeyVaultError(err, key); exit {
			return nil
		}
		return err
	}

	log.Debugf("Getting secret value for %s in Azure", key)
	if secretValue, err = vault.GetSecret(azureKeyVaultSecret); err != nil {
		msg := fmt.Sprintf(FailedAzureKeyVault, azureKeyVaultSecret.Name, azureKeyVaultSecret.Spec.Vault.Name)
		log.Warning(msg)
		h.recorder.Event(azureKeyVaultSecret, corev1.EventTypeWarning, ErrAzureVault, msg)
		return fmt.Errorf(msg)
	}

	secretHash := getMD5Hash(secretValue)

	log.Debugf("Checking if secret value for %s has changed in Azure", key)
	if azureKeyVaultSecret.Status.SecretHash != secretHash {
		log.Infof("Secret has changed in Azure Key Vault for AzureKeyvVaultSecret %s. Updating Secret now.", azureKeyVaultSecret.Name)

		newSecret, err := createNewSecret(azureKeyVaultSecret, &secretValue)
		if err != nil {
			msg := fmt.Sprintf(FailedAzureKeyVault, azureKeyVaultSecret.Name, azureKeyVaultSecret.Spec.Vault.Name)
			log.Error(msg)
			return fmt.Errorf(msg)
		}

		if secret, err = h.kubeclientset.CoreV1().Secrets(azureKeyVaultSecret.Namespace).Update(newSecret); err != nil {
			log.Warningf("Failed to create Secret, Error: %+v", err)
			return err
		}

		log.Debugf("Updating status for AzureKeyVaultSecret '%s'", azureKeyVaultSecret.Name)
		if err = h.updateAzureKeyVaultSecretStatus(azureKeyVaultSecret, secret); err != nil {
			return err
		}

		log.Warningf("Secret value will now change for Secret '%s'. Any resources (like Pods) using this Secrets must be restarted to pick up the new value. Details: https://github.com/kubernetes/kubernetes/issues/22368", azureKeyVaultSecret.Name)
		h.recorder.Event(azureKeyVaultSecret, corev1.EventTypeNormal, SuccessSynced, MessageResourceSyncedWithAzure)
	}

	return nil
}

func (h *Handler) getAzureKeyVaultSecret(key string) (*azureKeyVaultSecretv1alpha1.AzureKeyVaultSecret, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil, fmt.Errorf("invalid resource key: %s", key)
	}

	azureKeyVaultSecret, err := h.azureKeyVaultSecretsLister.AzureKeyVaultSecrets(namespace).Get(name)

	if err != nil {
		return nil, err
	}
	return azureKeyVaultSecret, err
}

func (h *Handler) getOrCreateKubernetesSecret(azureKeyVaultSecret *azureKeyVaultSecretv1alpha1.AzureKeyVaultSecret) (*corev1.Secret, error) {
	var secret *corev1.Secret
	var err error

	secretName := azureKeyVaultSecret.Spec.OutputSecret.Name
	if secretName == "" {
		return nil, fmt.Errorf("%s: secret name must be specified", azureKeyVaultSecret.Name)
	}

	if secret, err = h.secretsLister.Secrets(azureKeyVaultSecret.Namespace).Get(secretName); err != nil {
		if errors.IsNotFound(err) {
			var newSecret *corev1.Secret

			if newSecret, err = createNewSecret(azureKeyVaultSecret, nil); err != nil {
				msg := fmt.Sprintf(FailedAzureKeyVault, azureKeyVaultSecret.Name, azureKeyVaultSecret.Spec.Vault.Name)
				h.recorder.Event(azureKeyVaultSecret, corev1.EventTypeWarning, ErrAzureVault, msg)
				return nil, fmt.Errorf(msg)
			}

			if secret, err = h.kubeclientset.CoreV1().Secrets(azureKeyVaultSecret.Namespace).Create(newSecret); err != nil {
				return nil, err
			}

			log.Infof("Updating status for AzureKeyVaultSecret '%s'", azureKeyVaultSecret.Name)
			if err = h.updateAzureKeyVaultSecretStatus(azureKeyVaultSecret, secret); err != nil {
				return nil, err
			}

			return secret, nil
		}
	}

	return secret, err
}

func (h *Handler) updateAzureKeyVaultSecretStatus(azureKeyVaultSecret *azureKeyVaultSecretv1alpha1.AzureKeyVaultSecret, secret *corev1.Secret) error {
	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	azureKeyVaultSecretCopy := azureKeyVaultSecret.DeepCopy()
	secretValue := string(secret.Data[azureKeyVaultSecret.Spec.OutputSecret.KeyName])
	secretHash := getMD5Hash(secretValue)
	azureKeyVaultSecretCopy.Status.SecretHash = secretHash
	azureKeyVaultSecretCopy.Status.LastAzureUpdate = time.Now()

	// If the CustomResourceSubresources feature gate is not enabled,
	// we must use Update instead of UpdateStatus to update the Status block of the AzureKeyVaultSecret resource.
	// UpdateStatus will not allow changes to the Spec of the resource,
	// which is ideal for ensuring nothing other than resource status has been updated.
	_, err := h.azureKeyvaultClientset.AzurekeyvaultcontrollerV1alpha1().AzureKeyVaultSecrets(azureKeyVaultSecret.Namespace).UpdateStatus(azureKeyVaultSecretCopy)
	return err
}

func handleKeyVaultError(err error, key string) bool {
	log.Debugf("Handling error for '%s' in AzureKeyVaultSecret: %s", key, err.Error())
	if err != nil {
		// The AzureKeyVaultSecret resource may no longer exist, in which case we stop processing.
		if errors.IsNotFound(err) {
			log.Debugf("Error for '%s' was 'Not Found'", key)

			utilruntime.HandleError(fmt.Errorf("AzureKeyVaultSecret '%s' in work queue no longer exists", key))
			return true
		}
	}
	return false
}

// handleObject will take any resource implementing metav1.Object and attempt
// to find the AzureKeyVaultSecret resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that AzureKeyVaultSecret resource to be processed. If the object does not
// have an appropriate OwnerReference, it will simply be skipped.
func (h *Handler) handleObject(obj interface{}) (*azureKeyVaultSecretv1alpha1.AzureKeyVaultSecret, bool, error) {
	var object metav1.Object
	var ok bool

	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return nil, false, fmt.Errorf("error decoding object, invalid type")
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			return nil, false, fmt.Errorf("error decoding object tombstone, invalid type")
		}
		log.Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}

	log.Debugf("Processing object: %s", object.GetName())
	if ownerRef := metav1.GetControllerOf(object); ownerRef != nil {
		// If this object is not owned by a AzureKeyVaultSecret, we should not do anything more
		// with it.
		if ownerRef.Kind != "AzureKeyVaultSecret" {
			return nil, true, nil
		}

		azureKeyVaultSecret, err := h.azureKeyVaultSecretsLister.AzureKeyVaultSecrets(object.GetNamespace()).Get(ownerRef.Name)
		if err != nil {
			log.Infof("ignoring orphaned object '%s' of azureKeyVaultSecret '%s'", object.GetSelfLink(), ownerRef.Name)
			return nil, true, nil
		}

		return azureKeyVaultSecret, false, nil
	}
	return nil, true, nil
}

// newSecret creates a new Secret for a AzureKeyVaultSecret resource. It also sets
// the appropriate OwnerReferences on the resource so handleObject can discover
// the AzureKeyVaultSecret resource that 'owns' it.
func createNewSecret(azureKeyVaultSecret *azureKeyVaultSecretv1alpha1.AzureKeyVaultSecret, azureSecretValue *string) (*corev1.Secret, error) {
	var secretValue string

	if azureSecretValue == nil {
		var err error
		secretValue, err = vault.GetSecret(azureKeyVaultSecret)
		if err != nil {
			msg := fmt.Sprintf(FailedAzureKeyVault, azureKeyVaultSecret.Name, azureKeyVaultSecret.Spec.Vault.Name)
			return nil, fmt.Errorf(msg)
		}
	} else {
		secretValue = *azureSecretValue
	}

	stringData := make(map[string]string)
	stringData[azureKeyVaultSecret.Spec.OutputSecret.KeyName] = secretValue

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      azureKeyVaultSecret.Spec.OutputSecret.Name,
			Namespace: azureKeyVaultSecret.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(azureKeyVaultSecret, schema.GroupVersionKind{
					Group:   azureKeyVaultSecretv1alpha1.SchemeGroupVersion.Group,
					Version: azureKeyVaultSecretv1alpha1.SchemeGroupVersion.Version,
					Kind:    "AzureKeyVaultSecret",
				}),
			},
		},
		Type:       azureKeyVaultSecret.Spec.OutputSecret.Type,
		StringData: stringData,
	}, nil
}

func getMD5Hash(text string) string {
	hasher := md5.New()
	hasher.Write([]byte(text))
	return hex.EncodeToString(hasher.Sum(nil))
}

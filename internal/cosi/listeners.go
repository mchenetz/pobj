package cosi

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclientset "k8s.io/client-go/kubernetes"
	cosiapi "sigs.k8s.io/container-object-storage-interface-api/apis"
	objv1 "sigs.k8s.io/container-object-storage-interface-api/apis/objectstorage/v1alpha1"
	bucketclientset "sigs.k8s.io/container-object-storage-interface-api/client/clientset/versioned"
)

type Listener struct {
	DriverName string
	Endpoint   string
	Region     string
	CABundle   string
	Admin      *AdminClient
	Kube       kubeclientset.Interface
	Bucket     bucketclientset.Interface
}

func NewListener(driverName, endpoint, region, caBundle string, admin *AdminClient) *Listener {
	return &Listener{DriverName: driverName, Endpoint: endpoint, Region: region, CABundle: caBundle, Admin: admin}
}

func (l *Listener) InitializeKubeClient(c kubeclientset.Interface)     { l.Kube = c }
func (l *Listener) InitializeBucketClient(c bucketclientset.Interface) { l.Bucket = c }

func (l *Listener) Add(ctx context.Context, b *objv1.Bucket) error {
	if b.Spec.DriverName != l.DriverName || b.Status.BucketReady {
		return l.syncClaimReadyFromBucket(ctx, b)
	}
	bucketName := b.Name
	if b.Spec.ExistingBucketID != "" {
		bucketName = b.Spec.ExistingBucketID
	} else {
		if err := l.Admin.CreateBucket(ctx, bucketName); err != nil {
			return err
		}
	}
	copy := b.DeepCopy()
	copy.Status.BucketReady = true
	copy.Status.BucketID = bucketName
	updated, err := l.Bucket.ObjectstorageV1alpha1().Buckets().UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return l.syncClaimReadyFromBucket(ctx, updated)
}

func (l *Listener) Update(ctx context.Context, old *objv1.Bucket, new *objv1.Bucket) error {
	if err := l.Add(ctx, new); err != nil {
		return err
	}
	return l.syncClaimReadyFromBucket(ctx, new)
}

func (l *Listener) syncClaimReadyFromBucket(ctx context.Context, b *objv1.Bucket) error {
	if b.Spec.BucketClaim == nil || b.Spec.BucketClaim.Name == "" || b.Spec.BucketClaim.Namespace == "" {
		return nil
	}
	bc, err := l.Bucket.ObjectstorageV1alpha1().BucketClaims(b.Spec.BucketClaim.Namespace).Get(ctx, b.Spec.BucketClaim.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	copy := bc.DeepCopy()
	if copy.Status.BucketName == "" {
		copy.Status.BucketName = b.Name
	}
	copy.Status.BucketReady = b.Status.BucketReady
	_, err = l.Bucket.ObjectstorageV1alpha1().BucketClaims(copy.Namespace).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	return err
}

func (l *Listener) Delete(ctx context.Context, b *objv1.Bucket) error {
	if b.Spec.DriverName != l.DriverName {
		return nil
	}
	if b.Spec.DeletionPolicy == objv1.DeletionPolicyDelete {
		id := b.Status.BucketID
		if id == "" {
			id = b.Name
		}
		return l.Admin.DeleteBucket(ctx, id)
	}
	return nil
}

func (l *Listener) AddBucketClaim(ctx context.Context, bc *objv1.BucketClaim) error {
	className := bc.Spec.BucketClassName
	if className == "" {
		return fmt.Errorf("bucketClassName is required")
	}
	bucketClass, err := l.Bucket.ObjectstorageV1alpha1().BucketClasses().Get(ctx, className, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if bucketClass.DriverName != l.DriverName {
		return nil
	}

	bucketName := bc.Spec.ExistingBucketName
	if bucketName == "" {
		bucketName = claimBucketName(bc)
		if err := l.ensureClaimBucket(ctx, bc, bucketClass, bucketName); err != nil {
			return err
		}
	}

	b, err := l.Bucket.ObjectstorageV1alpha1().Buckets().Get(ctx, bucketName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	copy := bc.DeepCopy()
	copy.Status.BucketName = bucketName
	copy.Status.BucketReady = b.Status.BucketReady
	_, err = l.Bucket.ObjectstorageV1alpha1().BucketClaims(bc.Namespace).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	return err
}

func (l *Listener) UpdateBucketClaim(ctx context.Context, old *objv1.BucketClaim, new *objv1.BucketClaim) error {
	return l.AddBucketClaim(ctx, new)
}

func (l *Listener) DeleteBucketClaim(context.Context, *objv1.BucketClaim) error {
	return nil
}

func (l *Listener) ensureClaimBucket(ctx context.Context, bc *objv1.BucketClaim, bucketClass *objv1.BucketClass, bucketName string) error {
	if _, err := l.Bucket.ObjectstorageV1alpha1().Buckets().Get(ctx, bucketName, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	deletionPolicy := bucketClass.DeletionPolicy
	if deletionPolicy == "" {
		deletionPolicy = objv1.DeletionPolicyDelete
	}
	bucket := &objv1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: bucketName},
		Spec: objv1.BucketSpec{
			DriverName:      l.DriverName,
			BucketClassName: bucketClass.Name,
			Protocols:       bc.Spec.Protocols,
			Parameters:      bucketClass.Parameters,
			DeletionPolicy:  deletionPolicy,
			BucketClaim: &corev1.ObjectReference{
				Kind:       "BucketClaim",
				Namespace:  bc.Namespace,
				Name:       bc.Name,
				APIVersion: "objectstorage.k8s.io/v1alpha1",
				UID:        bc.UID,
			},
		},
	}
	_, err := l.Bucket.ObjectstorageV1alpha1().Buckets().Create(ctx, bucket, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func claimBucketName(bc *objv1.BucketClaim) string {
	raw := fmt.Sprintf("%s-%s-%s", bc.Namespace, bc.Name, string(bc.UID))
	raw = strings.ToLower(raw)
	repl := strings.NewReplacer("_", "-", ".", "-", "/", "-", ":", "-")
	out := repl.Replace(raw)
	if len(out) > 63 {
		out = out[:63]
	}
	out = strings.Trim(out, "-")
	if len(out) < 3 {
		out = out + "-pxobj"
	}
	return out
}

func (l *Listener) AddBucketAccess(ctx context.Context, b *objv1.BucketAccess) error {
	if b.Status.AccessGranted {
		return nil
	}
	bac, err := l.Bucket.ObjectstorageV1alpha1().BucketAccessClasses().Get(ctx, b.Spec.BucketAccessClassName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if bac.DriverName != l.DriverName {
		return nil
	}
	bc, err := l.Bucket.ObjectstorageV1alpha1().BucketClaims(b.Namespace).Get(ctx, b.Spec.BucketClaimName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if bc.Status.BucketName == "" {
		return fmt.Errorf("bucket claim %s is not bound yet", b.Spec.BucketClaimName)
	}
	bucket, err := l.Bucket.ObjectstorageV1alpha1().Buckets().Get(ctx, bc.Status.BucketName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !bucket.Status.BucketReady {
		return fmt.Errorf("bucket %s not ready", bucket.Name)
	}
	readOnly := false
	if v, ok := bac.Parameters["readonly"]; ok {
		parsed, _ := strconv.ParseBool(v)
		readOnly = parsed
	}
	creds, err := l.Admin.CreateAccess(ctx, bucket.Status.BucketID, readOnly)
	if err != nil {
		return err
	}
	if err := l.ensureSecret(ctx, b.Namespace, b.Spec.CredentialsSecretName, bucket.Status.BucketID, creds); err != nil {
		return err
	}
	copy := b.DeepCopy()
	copy.Status.AccessGranted = true
	copy.Status.AccountID = creds.AccessKey
	_, err = l.Bucket.ObjectstorageV1alpha1().BucketAccesses(b.Namespace).UpdateStatus(ctx, copy, metav1.UpdateOptions{})
	return err
}

func (l *Listener) UpdateBucketAccess(ctx context.Context, old *objv1.BucketAccess, new *objv1.BucketAccess) error {
	return l.AddBucketAccess(ctx, new)
}

func (l *Listener) DeleteBucketAccess(ctx context.Context, b *objv1.BucketAccess) error {
	if b.Status.AccountID != "" {
		if err := l.Admin.DeleteAccess(ctx, b.Status.AccountID); err != nil {
			return err
		}
	}
	if b.Spec.CredentialsSecretName != "" {
		_ = l.Kube.CoreV1().Secrets(b.Namespace).Delete(ctx, b.Spec.CredentialsSecretName, metav1.DeleteOptions{})
	}
	return nil
}

func (l *Listener) ensureSecret(ctx context.Context, ns, name, bucketName string, creds AccessKey) error {
	if name == "" {
		return fmt.Errorf("credentialsSecretName is required")
	}
	bucketInfo := cosiapi.BucketInfo{
		TypeMeta: metav1.TypeMeta{Kind: "BucketInfo", APIVersion: "objectstorage.k8s.io/v1alpha1"},
		Spec: cosiapi.BucketInfoSpec{
			BucketName:         bucketName,
			AuthenticationType: objv1.AuthenticationTypeKey,
			Protocols:          []objv1.Protocol{objv1.ProtocolS3},
			S3: &cosiapi.SecretS3{
				Endpoint:        l.Endpoint,
				Region:          l.Region,
				AccessKeyID:     creds.AccessKey,
				AccessSecretKey: creds.SecretKey,
			},
		},
	}
	raw, _ := json.Marshal(bucketInfo)

	data := map[string]string{
		"BUCKET_NAME":           bucketName,
		"BUCKET_HOST":           l.Endpoint,
		"AWS_REGION":            l.Region,
		"AWS_ACCESS_KEY_ID":     creds.AccessKey,
		"AWS_SECRET_ACCESS_KEY": creds.SecretKey,
		"COSI_BUCKET_INFO":      string(raw),
	}
	if l.CABundle != "" {
		data["AWS_CA_BUNDLE_PEM"] = l.CABundle
	}

	existing, err := l.Kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		s := &corev1.Secret{}
		s.Namespace = ns
		s.Name = name
		s.StringData = data
		_, err = l.Kube.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	existing.StringData = data
	_, err = l.Kube.CoreV1().Secrets(ns).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

type BucketAccessListenerAdapter struct{ *Listener }

func (a BucketAccessListenerAdapter) InitializeKubeClient(c kubeclientset.Interface) {
	a.Listener.InitializeKubeClient(c)
}
func (a BucketAccessListenerAdapter) InitializeBucketClient(c bucketclientset.Interface) {
	a.Listener.InitializeBucketClient(c)
}
func (a BucketAccessListenerAdapter) Add(ctx context.Context, b *objv1.BucketAccess) error {
	return a.Listener.AddBucketAccess(ctx, b)
}
func (a BucketAccessListenerAdapter) Update(ctx context.Context, old *objv1.BucketAccess, new *objv1.BucketAccess) error {
	return a.Listener.UpdateBucketAccess(ctx, old, new)
}
func (a BucketAccessListenerAdapter) Delete(ctx context.Context, b *objv1.BucketAccess) error {
	return a.Listener.DeleteBucketAccess(ctx, b)
}

type BucketClaimListenerAdapter struct{ *Listener }

func (a BucketClaimListenerAdapter) InitializeKubeClient(c kubeclientset.Interface) {
	a.Listener.InitializeKubeClient(c)
}
func (a BucketClaimListenerAdapter) InitializeBucketClient(c bucketclientset.Interface) {
	a.Listener.InitializeBucketClient(c)
}
func (a BucketClaimListenerAdapter) Add(ctx context.Context, b *objv1.BucketClaim) error {
	return a.Listener.AddBucketClaim(ctx, b)
}
func (a BucketClaimListenerAdapter) Update(ctx context.Context, old *objv1.BucketClaim, new *objv1.BucketClaim) error {
	return a.Listener.UpdateBucketClaim(ctx, old, new)
}
func (a BucketClaimListenerAdapter) Delete(ctx context.Context, b *objv1.BucketClaim) error {
	return a.Listener.DeleteBucketClaim(ctx, b)
}

type NoopBucketClassListener struct{}

func (n NoopBucketClassListener) InitializeKubeClient(kubeclientset.Interface)     {}
func (n NoopBucketClassListener) InitializeBucketClient(bucketclientset.Interface) {}
func (n NoopBucketClassListener) Add(context.Context, *objv1.BucketClass) error    { return nil }
func (n NoopBucketClassListener) Update(context.Context, *objv1.BucketClass, *objv1.BucketClass) error {
	return nil
}
func (n NoopBucketClassListener) Delete(context.Context, *objv1.BucketClass) error { return nil }

type NoopBucketAccessClassListener struct{}

func (n NoopBucketAccessClassListener) InitializeKubeClient(kubeclientset.Interface)     {}
func (n NoopBucketAccessClassListener) InitializeBucketClient(bucketclientset.Interface) {}
func (n NoopBucketAccessClassListener) Add(context.Context, *objv1.BucketAccessClass) error {
	return nil
}
func (n NoopBucketAccessClassListener) Update(context.Context, *objv1.BucketAccessClass, *objv1.BucketAccessClass) error {
	return nil
}
func (n NoopBucketAccessClassListener) Delete(context.Context, *objv1.BucketAccessClass) error {
	return nil
}

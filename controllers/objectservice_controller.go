package controllers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	pxv1 "github.com/mchenetz/pxobj/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type ObjectServiceReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	OperatorImage string
}

func (r *ObjectServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := &pxv1.ObjectService{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if obj.Spec.Replicas <= 0 {
		obj.Spec.Replicas = 1
	}
	if obj.Spec.Port == 0 {
		obj.Spec.Port = 9000
	}
	if obj.Spec.ServiceType == "" {
		obj.Spec.ServiceType = string(corev1.ServiceTypeClusterIP)
	}
	if obj.Spec.DataPath == "" {
		obj.Spec.DataPath = "/data"
	}
	if obj.Spec.VolumeSize == "" {
		obj.Spec.VolumeSize = "100Gi"
	}
	if obj.Spec.AdminSecretName == "" {
		obj.Spec.AdminSecretName = obj.Name + "-admin"
	}
	if obj.Spec.TLSSecretName == "" {
		obj.Spec.TLSSecretName = obj.Name + "-tls"
	}

	if err := r.ensureAdminSecret(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureTLS(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureHeadlessService(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureService(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureStatefulSet(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureCOSIDeployment(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}

	endpoint := fmt.Sprintf("%s.%s.svc.cluster.local:%d", obj.Name, obj.Namespace, obj.Spec.Port)
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}, sts); err == nil {
		obj.Status.ReadyReplicas = sts.Status.ReadyReplicas
	}
	obj.Status.Phase = "Ready"
	obj.Status.ServiceEndpoint = endpoint
	obj.Status.ObservedGeneration = obj.Generation
	if err := r.Status().Update(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ObjectServiceReconciler) ensureTLS(ctx context.Context, obj *pxv1.ObjectService) error {
	if obj.Spec.UseCertManager {
		if err := r.ensureCertManagerCertificate(ctx, obj); err != nil {
			return err
		}
	}
	tlsSecret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: obj.Spec.TLSSecretName, Namespace: obj.Namespace}, tlsSecret)
	if errors.IsNotFound(err) {
		if obj.Spec.UseCertManager {
			return fmt.Errorf("waiting for cert-manager to create TLS secret %s", obj.Spec.TLSSecretName)
		}
		return r.createOrRotateSelfSignedTLSSecret(ctx, obj, nil)
	}
	if err != nil {
		return err
	}
	if obj.Spec.UseCertManager {
		if _, ok := tlsSecret.Data["ca.crt"]; !ok {
			return fmt.Errorf("TLS secret %s is missing ca.crt required for internal trust", obj.Spec.TLSSecretName)
		}
		return nil
	}
	return r.createOrRotateSelfSignedTLSSecret(ctx, obj, tlsSecret)
}

func (r *ObjectServiceReconciler) ensureCertManagerCertificate(ctx context.Context, obj *pxv1.ObjectService) error {
	issuerKind := obj.Spec.IssuerRefKind
	if issuerKind == "" {
		issuerKind = "Issuer"
	}
	issuerGroup := obj.Spec.IssuerRefGroup
	if issuerGroup == "" {
		issuerGroup = "cert-manager.io"
	}
	if obj.Spec.IssuerRefName == "" {
		return fmt.Errorf("issuerRefName is required when useCertManager=true")
	}
	headless := obj.Name + "-headless"
	dnsNames := []any{
		obj.Name,
		fmt.Sprintf("%s.%s", obj.Name, obj.Namespace),
		fmt.Sprintf("%s.%s.svc", obj.Name, obj.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", obj.Name, obj.Namespace),
		fmt.Sprintf("*.%s.%s.svc.cluster.local", headless, obj.Namespace),
	}

	cert := &unstructured.Unstructured{}
	cert.SetAPIVersion("cert-manager.io/v1")
	cert.SetKind("Certificate")
	cert.SetName(obj.Name + "-tls")
	cert.SetNamespace(obj.Namespace)
	_ = unstructured.SetNestedField(cert.Object, obj.Spec.TLSSecretName, "spec", "secretName")
	_ = unstructured.SetNestedSlice(cert.Object, dnsNames, "spec", "dnsNames")
	_ = unstructured.SetNestedField(cert.Object, true, "spec", "isCA")
	_ = unstructured.SetNestedMap(cert.Object, map[string]any{
		"name":  obj.Spec.IssuerRefName,
		"kind":  issuerKind,
		"group": issuerGroup,
	}, "spec", "issuerRef")
	_ = unstructured.SetNestedMap(cert.Object, map[string]any{
		"algorithm": "RSA",
		"size":      int64(2048),
	}, "spec", "privateKey")
	_ = unstructured.SetNestedSlice(cert.Object, []any{"server auth", "client auth"}, "spec", "usages")
	_ = unstructured.SetNestedField(cert.Object, "2160h", "spec", "duration")
	_ = unstructured.SetNestedField(cert.Object, "720h", "spec", "renewBefore")

	existing := &unstructured.Unstructured{}
	existing.SetAPIVersion("cert-manager.io/v1")
	existing.SetKind("Certificate")
	err := r.Get(ctx, types.NamespacedName{Name: cert.GetName(), Namespace: cert.GetNamespace()}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, cert)
	}
	if err != nil {
		return err
	}
	cert.SetResourceVersion(existing.GetResourceVersion())
	return r.Update(ctx, cert)
}

func (r *ObjectServiceReconciler) createOrRotateSelfSignedTLSSecret(ctx context.Context, obj *pxv1.ObjectService, existing *corev1.Secret) error {
	needRotate := existing == nil
	if existing != nil {
		crt := existing.Data["tls.crt"]
		key := existing.Data["tls.key"]
		ca := existing.Data["ca.crt"]
		if len(crt) == 0 || len(key) == 0 || len(ca) == 0 {
			needRotate = true
		} else if cert, err := parseLeafCert(crt); err != nil {
			needRotate = true
		} else if time.Until(cert.NotAfter) < (30 * 24 * time.Hour) {
			needRotate = true
		}
	}
	if !needRotate {
		return nil
	}

	headless := obj.Name + "-headless"
	dns := []string{
		obj.Name,
		fmt.Sprintf("%s.%s", obj.Name, obj.Namespace),
		fmt.Sprintf("%s.%s.svc", obj.Name, obj.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", obj.Name, obj.Namespace),
		fmt.Sprintf("*.%s.%s.svc.cluster.local", headless, obj.Namespace),
	}
	caCrtPEM, caKeyPEM, err := newCA(obj.Name + "-pxobj-ca")
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := newLeafCert(obj.Name+"-pxobj", dns, caCrtPEM, caKeyPEM)
	if err != nil {
		return err
	}

	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: obj.Spec.TLSSecretName, Namespace: obj.Namespace}}
	if existing != nil {
		s.ResourceVersion = existing.ResourceVersion
	}
	s.Type = corev1.SecretTypeTLS
	s.Data = map[string][]byte{
		"tls.crt": certPEM,
		"tls.key": keyPEM,
		"ca.crt":  caCrtPEM,
	}
	if err := controllerutil.SetControllerReference(obj, s, r.Scheme); err != nil {
		return err
	}
	if existing == nil {
		return r.Create(ctx, s)
	}
	return r.Update(ctx, s)
}

func parseLeafCert(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("invalid cert pem")
	}
	return x509.ParseCertificate(block.Bytes)
}

func newCA(cn string) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

func newLeafCert(cn string, dns []string, caCertPEM, caKeyPEM []byte) ([]byte, []byte, error) {
	caBlock, _ := pem.Decode(caCertPEM)
	if caBlock == nil {
		return nil, nil, fmt.Errorf("invalid ca cert")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		return nil, nil, fmt.Errorf("invalid ca key")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		DNSNames:              dns,
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(180 * 24 * time.Hour),
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM, nil
}

func (r *ObjectServiceReconciler) ensureAdminSecret(ctx context.Context, obj *pxv1.ObjectService) error {
	s := &corev1.Secret{}
	nn := types.NamespacedName{Name: obj.Spec.AdminSecretName, Namespace: obj.Namespace}
	if err := r.Get(ctx, nn, s); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	tok, err := randomHex(32)
	if err != nil {
		return err
	}
	s = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: obj.Spec.AdminSecretName, Namespace: obj.Namespace},
		StringData: map[string]string{"adminToken": tok},
	}
	if err := controllerutil.SetControllerReference(obj, s, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, s)
}

func (r *ObjectServiceReconciler) ensureHeadlessService(ctx context.Context, obj *pxv1.ObjectService) error {
	name := obj.Name + "-headless"
	svc := &corev1.Service{}
	nn := types.NamespacedName{Name: name, Namespace: obj.Namespace}
	err := r.Get(ctx, nn, svc)
	ports := []corev1.ServicePort{
		{Name: "s3", Port: obj.Spec.Port, TargetPort: intstr.FromInt(int(obj.Spec.Port))},
		{Name: "admin", Port: 19000, TargetPort: intstr.FromInt(19000)},
	}
	if errors.IsNotFound(err) {
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: obj.Namespace, Labels: map[string]string{"app": obj.Name}},
			Spec: corev1.ServiceSpec{
				ClusterIP: "None",
				Ports:     ports,
				Selector:  map[string]string{"app": obj.Name},
			},
		}
		if err := controllerutil.SetControllerReference(obj, svc, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}
	svc.Spec.ClusterIP = "None"
	svc.Spec.Ports = ports
	svc.Spec.Selector = map[string]string{"app": obj.Name}
	return r.Update(ctx, svc)
}

func (r *ObjectServiceReconciler) ensureService(ctx context.Context, obj *pxv1.ObjectService) error {
	svc := &corev1.Service{}
	nn := types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}
	err := r.Get(ctx, nn, svc)
	ports := []corev1.ServicePort{
		{Name: "s3", Port: obj.Spec.Port, TargetPort: intstr.FromInt(int(obj.Spec.Port))},
		{Name: "admin", Port: 19000, TargetPort: intstr.FromInt(19000)},
	}
	if errors.IsNotFound(err) {
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: obj.Name, Namespace: obj.Namespace, Labels: map[string]string{"app": obj.Name}},
			Spec: corev1.ServiceSpec{
				Type:     corev1.ServiceType(obj.Spec.ServiceType),
				Ports:    ports,
				Selector: map[string]string{"app": obj.Name},
			},
		}
		if err := controllerutil.SetControllerReference(obj, svc, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}

	svc.Spec.Type = corev1.ServiceType(obj.Spec.ServiceType)
	svc.Spec.Ports = ports
	svc.Spec.Selector = map[string]string{"app": obj.Name}
	return r.Update(ctx, svc)
}

func (r *ObjectServiceReconciler) ensureStatefulSet(ctx context.Context, obj *pxv1.ObjectService) error {
	sts := &appsv1.StatefulSet{}
	nn := types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace}
	err := r.Get(ctx, nn, sts)

	qty, errQ := resource.ParseQuantity(obj.Spec.VolumeSize)
	if errQ != nil {
		return fmt.Errorf("invalid volumeSize %q: %w", obj.Spec.VolumeSize, errQ)
	}

	labels := map[string]string{"app": obj.Name}
	replicas := obj.Spec.Replicas
	mountPath := obj.Spec.DataPath
	headless := obj.Name + "-headless"
	tlsDir := "/etc/pxobj/tls"

	template := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: obj.Name, Namespace: obj.Namespace},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: headless,
			Replicas:    &replicas,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "objectd",
						Image:   r.OperatorImage,
						Command: []string{"/pxobj-objectd"},
						Ports:   []corev1.ContainerPort{{ContainerPort: obj.Spec.Port, Name: "s3"}, {ContainerPort: 19000, Name: "admin"}},
						Env: []corev1.EnvVar{
							{Name: "PXOBJ_DATA_DIR", Value: mountPath},
							{Name: "PXOBJ_S3_PORT", Value: fmt.Sprintf("%d", obj.Spec.Port)},
							{Name: "PXOBJ_ADMIN_PORT", Value: "19000"},
							{Name: "PXOBJ_SERVICE_NAME", Value: obj.Name},
							{Name: "PXOBJ_HEADLESS_SERVICE_NAME", Value: headless},
							{Name: "PXOBJ_REPLICAS", Value: fmt.Sprintf("%d", obj.Spec.Replicas)},
							{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
							{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
							{Name: "PXOBJ_TLS_ENABLED", Value: "true"},
							{Name: "PXOBJ_TLS_CERT_FILE", Value: tlsDir + "/tls.crt"},
							{Name: "PXOBJ_TLS_KEY_FILE", Value: tlsDir + "/tls.key"},
							{Name: "PXOBJ_TLS_CA_FILE", Value: tlsDir + "/ca.crt"},
							{Name: "PXOBJ_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: obj.Spec.AdminSecretName}, Key: "adminToken"}}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "data", MountPath: mountPath},
							{Name: "tls", MountPath: tlsDir, ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{{
						Name:         "tls",
						VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: obj.Spec.TLSSecretName}},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:        corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: qty}},
					StorageClassName: &obj.Spec.StorageClassName,
				},
			}},
		},
	}

	if errors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(obj, &template, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, &template)
	}
	if err != nil {
		return err
	}

	sts.Spec.Replicas = template.Spec.Replicas
	sts.Spec.Template = template.Spec.Template
	sts.Spec.ServiceName = template.Spec.ServiceName
	sts.Spec.VolumeClaimTemplates = template.Spec.VolumeClaimTemplates
	return r.Update(ctx, sts)
}

func (r *ObjectServiceReconciler) ensureCOSIDeployment(ctx context.Context, obj *pxv1.ObjectService) error {
	name := obj.Name + "-cosi"
	dep := &appsv1.Deployment{}
	nn := types.NamespacedName{Name: name, Namespace: obj.Namespace}
	err := r.Get(ctx, nn, dep)

	replicas := int32(1)
	labels := map[string]string{"app": name}
	endpoint := fmt.Sprintf("%s.%s.svc.cluster.local:%d", obj.Name, obj.Namespace, obj.Spec.Port)
	adminURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:19000", obj.Name, obj.Namespace)
	template := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: obj.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "pxobj-cosi-driver",
					Containers: []corev1.Container{{
						Name:    "cosidriver",
						Image:   r.OperatorImage,
						Command: []string{"/pxobj-cosidriver"},
						Env: []corev1.EnvVar{
							{Name: "PXOBJ_DRIVER_NAME", Value: "pxobj.io/s3"},
							{Name: "PXOBJ_S3_ENDPOINT", Value: endpoint},
							{Name: "PXOBJ_S3_REGION", Value: "us-east-1"},
							{Name: "PXOBJ_S3_CA_PEM", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: obj.Spec.TLSSecretName}, Key: "ca.crt"}}},
							{Name: "PXOBJ_ADMIN_URL", Value: adminURL},
							{Name: "PXOBJ_ADMIN_CA_PEM", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: obj.Spec.TLSSecretName}, Key: "ca.crt"}}},
							{Name: "PXOBJ_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: obj.Spec.AdminSecretName}, Key: "adminToken"}}},
							{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
							{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
						},
					}},
				},
			},
		},
	}
	if errors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(obj, &template, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, &template)
	}
	if err != nil {
		return err
	}
	dep.Spec = template.Spec
	return r.Update(ctx, dep)
}

func (r *ObjectServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pxv1.ObjectService{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

func randomHex(bytesN int) (string, error) {
	b := make([]byte, bytesN)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

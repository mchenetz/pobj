package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type ObjectServiceSpec struct {
	Replicas         int32  `json:"replicas"`
	StorageClassName string `json:"storageClassName"`
	VolumeSize       string `json:"volumeSize"`
	ServiceType      string `json:"serviceType,omitempty"`
	Port             int32  `json:"port,omitempty"`

	TLSSecretName   string `json:"tlsSecretName,omitempty"`
	UseCertManager  bool   `json:"useCertManager,omitempty"`
	IssuerRefName   string `json:"issuerRefName,omitempty"`
	IssuerRefKind   string `json:"issuerRefKind,omitempty"`
	IssuerRefGroup  string `json:"issuerRefGroup,omitempty"`
	AdminSecretName string `json:"adminSecretName,omitempty"`

	DataPath         string `json:"dataPath,omitempty"`
	EnableVersioning bool   `json:"enableVersioning,omitempty"`
	ForcePathStyle   bool   `json:"forcePathStyle,omitempty"`
}

type ObjectServiceStatus struct {
	Phase              string `json:"phase,omitempty"`
	ReadyReplicas      int32  `json:"readyReplicas,omitempty"`
	ServiceEndpoint    string `json:"serviceEndpoint,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type ObjectService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ObjectServiceSpec   `json:"spec,omitempty"`
	Status ObjectServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ObjectServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectService `json:"items"`
}

func (in *ObjectService) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(ObjectService)
	*out = *in
	out.TypeMeta = in.TypeMeta
	out.ObjectMeta = *in.ObjectMeta.DeepCopy()
	return out
}

func (in *ObjectServiceList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(ObjectServiceList)
	*out = *in
	out.TypeMeta = in.TypeMeta
	out.ListMeta = in.ListMeta
	if in.Items != nil {
		out.Items = make([]ObjectService, len(in.Items))
		copy(out.Items, in.Items)
	}
	return out
}

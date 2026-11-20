package main

// The two kinds this issuer adds: LabIssuer (namespaced) and LabClusterIssuer (cluster-scoped), in the
// group issuer.lab.example. The deep copies are written by hand, so the build needs no code generator;
// the CustomResourceDefinitions in deploy/crds.yaml are written by hand too.

import (
	"github.com/cert-manager/issuer-lib/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var groupVersion = schema.GroupVersion{Group: "issuer.lab.example", Version: "v1alpha1"}

// LabIssuerSpec names the Secret that holds the CA: tls.crt, tls.key and, if it has one, ca.crt.
// A LabIssuer reads the Secret in its own namespace; a LabClusterIssuer reads it in the namespace the
// flag -cluster-resource-namespace names.
type LabIssuerSpec struct {
	SecretName string `json:"secretName"`
}

type LabIssuer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              LabIssuerSpec         `json:"spec"`
	Status            v1alpha1.IssuerStatus `json:"status,omitzero"`
}

type LabClusterIssuer LabIssuer

type LabIssuerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabIssuer `json:"items"`
}

type LabClusterIssuerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LabClusterIssuer `json:"items"`
}

// What issuer-lib asks of an issuer kind: its conditions, and the name cert-manager's approver knows
// it by. That name is the "signer" a ClusterRole grants the verb approve on (deploy/approve-rbac.yaml).
func (i *LabIssuer) GetConditions() []metav1.Condition        { return i.Status.Conditions }
func (i *LabIssuer) GetIssuerTypeIdentifier() string          { return "labissuers.issuer.lab.example" }
func (i *LabClusterIssuer) GetConditions() []metav1.Condition { return i.Status.Conditions }
func (i *LabClusterIssuer) GetIssuerTypeIdentifier() string {
	return "labclusterissuers.issuer.lab.example"
}

func (i *LabIssuer) DeepCopyObject() runtime.Object {
	o := *i
	i.ObjectMeta.DeepCopyInto(&o.ObjectMeta)
	i.Status.DeepCopyInto(&o.Status)
	return &o
}

func (i *LabClusterIssuer) DeepCopyObject() runtime.Object {
	o := *i
	i.ObjectMeta.DeepCopyInto(&o.ObjectMeta)
	i.Status.DeepCopyInto(&o.Status)
	return &o
}

func (l *LabIssuerList) DeepCopyObject() runtime.Object {
	o := *l
	l.ListMeta.DeepCopyInto(&o.ListMeta)
	o.Items = make([]LabIssuer, len(l.Items))
	for n := range l.Items {
		o.Items[n] = *l.Items[n].DeepCopyObject().(*LabIssuer)
	}
	return &o
}

func (l *LabClusterIssuerList) DeepCopyObject() runtime.Object {
	o := *l
	l.ListMeta.DeepCopyInto(&o.ListMeta)
	o.Items = make([]LabClusterIssuer, len(l.Items))
	for n := range l.Items {
		o.Items[n] = *l.Items[n].DeepCopyObject().(*LabClusterIssuer)
	}
	return &o
}

func addToScheme(s *runtime.Scheme) {
	s.AddKnownTypes(groupVersion, &LabIssuer{}, &LabIssuerList{}, &LabClusterIssuer{}, &LabClusterIssuerList{})
	metav1.AddToGroupVersion(s, groupVersion)
}

/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package resources

import (
	"testing"

	"knative.dev/pkg/kmeta"

	"github.com/google/go-cmp/cmp"
	netv1alpha1 "github.com/knative/serving/pkg/apis/networking/v1alpha1"
	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var route = &v1alpha1.Route{
	ObjectMeta: metav1.ObjectMeta{
		Name:      "route",
		Namespace: "default",
		UID:       "12345",
	},
}

var dnsNameTagMap = map[string]string{
	"v1.default.example.com":         "",
	"v1-current.default.example.com": "current",
}

func TestMakeCertificates(t *testing.T) {
	want := []*netv1alpha1.Certificate{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "route-12345-200999684",
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(route)},
			},
			Spec: netv1alpha1.CertificateSpec{
				DNSNames:   []string{"v1-current.default.example.com"},
				SecretName: "route-12345-200999684",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "route-12345",
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(route)},
			},
			Spec: netv1alpha1.CertificateSpec{
				DNSNames:   []string{"v1.default.example.com"},
				SecretName: "route-12345",
			},
		},
	}
	got := MakeCertificates(route, dnsNameTagMap)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("MakeCertificate (-want, +got) = %v", diff)
	}
}

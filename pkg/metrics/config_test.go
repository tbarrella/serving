/*
Copyright 2018 The Knative Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"knative.dev/pkg/metrics"
	"knative.dev/pkg/system"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "knative.dev/pkg/configmap/testing"
	_ "knative.dev/pkg/system/testing"
)

func TestOurObservability(t *testing.T) {
	cm, example := ConfigMapsFromTestFile(t, metrics.ConfigMapName())

	if _, err := NewObservabilityConfigFromConfigMap(cm); err != nil {
		t.Errorf("NewObservabilityFromConfigMap(actual) = %v", err)
	}

	if _, err := NewObservabilityConfigFromConfigMap(example); err != nil {
		t.Errorf("NewObservabilityFromConfigMap(example) = %v", err)
	}
}

func TestObservabilityConfiguration(t *testing.T) {
	observabilityConfigTests := []struct {
		name           string
		wantErr        bool
		wantController interface{}
		config         *corev1.ConfigMap
	}{{
		name:    "observability configuration with all inputs",
		wantErr: false,
		wantController: &ObservabilityConfig{
			LoggingURLTemplate:     "https://logging.io",
			EnableVarLogCollection: true,
			RequestLogTemplate:     `{"requestMethod": "{{.Request.Method}}"}`,
			RequestMetricsBackend:  "stackdriver",
		},
		config: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: system.Namespace(),
				Name:      metrics.ConfigMapName(),
			},
			Data: map[string]string{
				"logging.enable-var-log-collection":           "true",
				"logging.revision-url-template":               "https://logging.io",
				"logging.write-request-logs":                  "true",
				"logging.request-log-template":                `{"requestMethod": "{{.Request.Method}}"}`,
				"metrics.request-metrics-backend-destination": "stackdriver",
			},
		},
	}, {
		name:    "observability config with no map",
		wantErr: false,
		wantController: &ObservabilityConfig{
			EnableVarLogCollection: false,
			LoggingURLTemplate:     defaultLogURLTemplate,
			RequestLogTemplate:     "",
			RequestMetricsBackend:  "",
		},
		config: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: system.Namespace(),
				Name:      metrics.ConfigMapName(),
			},
		},
	}, {
		name:           "invalid request log template",
		wantErr:        true,
		wantController: (*ObservabilityConfig)(nil),
		config: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: system.Namespace(),
				Name:      metrics.ConfigMapName(),
			},
			Data: map[string]string{
				"logging.request-log-template": `{{ something }}`,
			},
		},
	}}

	for _, tt := range observabilityConfigTests {
		t.Run(tt.name, func(t *testing.T) {
			actualController, err := NewObservabilityConfigFromConfigMap(tt.config)

			if (err != nil) != tt.wantErr {
				t.Fatalf("Test: %q; NewObservabilityFromConfigMap() error = %v, WantErr %v", tt.name, err, tt.wantErr)
			}

			if diff := cmp.Diff(actualController, tt.wantController); diff != "" {
				t.Fatalf("Test: %q; want %v, but got %v", tt.name, tt.wantController, actualController)
			}
		})
	}
}

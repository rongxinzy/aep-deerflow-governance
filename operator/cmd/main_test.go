package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnvHelpers(t *testing.T) {
	t.Setenv("OP_STRING", "value")
	t.Setenv("OP_INT", "42")
	t.Setenv("OP_BAD_INT", "x")

	assert.Equal(t, "value", envOrDefault("OP_STRING", "fallback"))
	assert.Equal(t, "fallback", envOrDefault("OP_MISSING", "fallback"))
	assert.Equal(t, 42, envIntOrDefault("OP_INT", 7))
	assert.Equal(t, 7, envIntOrDefault("OP_BAD_INT", 7))
	assert.Equal(t, 7, envIntOrDefault("OP_MISSING", 7))
	assert.Equal(t, 9, envIntOrDefault("OP_ZERO_IS_DEFAULT", 9))
}

func TestReadSecretKey(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns"},
		Data:       map[string][]byte{"KEY": []byte(" value ")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	value, err := readSecretKey(context.Background(), c, "ns", "creds", "KEY")
	require.NoError(t, err)
	assert.Equal(t, "value", value, "whitespace is trimmed")

	_, err = readSecretKey(context.Background(), c, "ns", "", "KEY")
	assert.ErrorIs(t, err, errNoCredentialsSecret)

	_, err = readSecretKey(context.Background(), c, "ns", "creds", "MISSING")
	assert.ErrorIs(t, err, errMissingKey)

	_, err = readSecretKey(context.Background(), c, "ns", "no-such-secret", "KEY")
	assert.Error(t, err)
}

func TestExitErrors(t *testing.T) {
	assert.Equal(t, "spec.credentials.name must reference the platform credentials secret", errNoCredentialsSecret.Error())
	assert.Equal(t, "credentials secret is missing OPENVIKING_ROOT_API_KEY", errMissingKey.Error())
}

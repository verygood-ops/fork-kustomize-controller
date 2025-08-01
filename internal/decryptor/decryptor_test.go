/*
Copyright 2021 The Flux authors

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

package decryptor

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	extage "filippo.io/age"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	. "github.com/onsi/gomega"
	gt "github.com/onsi/gomega/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/provider"
	"sigs.k8s.io/kustomize/api/resource"
	kustypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"

	"github.com/fluxcd/pkg/apis/meta"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1"
)

func TestIsEncryptedSecret(t *testing.T) {
	tests := []struct {
		name   string
		object []byte
		want   gt.GomegaMatcher
	}{
		{name: "encrypted secret", object: []byte("apiVersion: v1\nkind: Secret\nsops: true\n"), want: BeTrue()},
		{name: "decrypted secret", object: []byte("apiVersion: v1\nkind: Secret\n"), want: BeFalse()},
		{name: "other resource", object: []byte("apiVersion: v1\nkind: Deployment\n"), want: BeFalse()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			u := &unstructured.Unstructured{}
			g.Expect(yaml.Unmarshal(tt.object, u)).To(Succeed())
			g.Expect(IsEncryptedSecret(u)).To(tt.want)
		})
	}
}

func TestDecryptor_ImportKeys(t *testing.T) {
	g := NewWithT(t)

	const provider = "sops"

	pgpKey, err := os.ReadFile("testdata/pgp.asc")
	g.Expect(err).ToNot(HaveOccurred())
	ageKey, err := os.ReadFile("testdata/age.txt")
	g.Expect(err).ToNot(HaveOccurred())

	tests := []struct {
		name        string
		decryption  *kustomizev1.Decryption
		secret      *corev1.Secret
		wantErr     bool
		inspectFunc func(g *GomegaWithT, decryptor *Decryptor)
	}{
		{
			name: "PGP key",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "pgp-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pgp-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"pgp" + DecryptionPGPExt: pgpKey,
				},
			},
		},
		{
			name: "PGP key import error",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "pgp-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pgp-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"pgp" + DecryptionPGPExt: []byte("not-a-valid-armored-key"),
				},
			},
			wantErr: true,
		},
		{
			name: "age key",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "age-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "age-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"age" + DecryptionAgeExt: ageKey,
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *Decryptor) {
				g.Expect(decryptor.ageIdentities).To(HaveLen(1))
			},
		},
		{
			name: "age key import error",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "age-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "age-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"age" + DecryptionAgeExt: []byte("not-a-valid-key"),
				},
			},
			wantErr: true,
			inspectFunc: func(g *GomegaWithT, decryptor *Decryptor) {
				g.Expect(decryptor.ageIdentities).To(HaveLen(0))
			},
		},
		{
			name: "HC Vault token",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "hcvault-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hcvault-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionVaultTokenFileName: []byte("some-hcvault-token"),
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *Decryptor) {
				g.Expect(decryptor.vaultToken).To(Equal("some-hcvault-token"))
			},
		},
		{
			name: "AWS KMS credentials",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "awskms-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "awskms-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionAWSKmsFile: []byte(`aws_access_key_id: test-id
aws_secret_access_key: test-secret
aws_session_token: test-token`),
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *Decryptor) {
				g.Expect(decryptor.awsCredentialsProvider).ToNot(BeNil())
			},
		},
		{
			name: "GCP Service Account key",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "gcpkms-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "gcpkms-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionGCPCredsFile: []byte(`{ "client_id": "<client-id>.apps.googleusercontent.com",
					"client_secret": "<secret>",
				   "type": "authorized_user"}`),
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *Decryptor) {
				g.Expect(decryptor.gcpTokenSource).ToNot(BeNil())
			},
		},
		{
			name: "Azure Key Vault token",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "azkv-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "azkv-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionAzureAuthFile: []byte(`tenantId: some-tenant-id
clientId: some-client-id
clientSecret: some-client-secret`),
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *Decryptor) {
				g.Expect(decryptor.azureTokenCredential).ToNot(BeNil())
			},
		},
		{
			name: "Azure Key Vault token load config error",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "azkv-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "azkv-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionAzureAuthFile: []byte(`{"malformed\: JSON"}`),
				},
			},
			wantErr: true,
			inspectFunc: func(g *GomegaWithT, decryptor *Decryptor) {
				g.Expect(decryptor.azureTokenCredential).To(BeNil())
			},
		},
		{
			name: "Azure Key Vault unsupported config",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "azkv-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "azkv-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionAzureAuthFile: []byte(`tenantId: incomplete`),
				},
			},
			wantErr: true,
			inspectFunc: func(g *GomegaWithT, decryptor *Decryptor) {
				g.Expect(decryptor.azureTokenCredential).To(BeNil())
			},
		},
		{
			name: "multiple Secret data entries",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "multiple-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multiple-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"age" + DecryptionAgeExt:     ageKey,
					DecryptionVaultTokenFileName: []byte("some-hcvault-token"),
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *Decryptor) {
				g.Expect(decryptor.vaultToken).ToNot(BeEmpty())
				g.Expect(decryptor.ageIdentities).To(HaveLen(1))
			},
		},
		{
			name:       "no Decryption spec",
			decryption: nil,
			wantErr:    false,
		},
		{
			name: "no Decryption Secret",
			decryption: &kustomizev1.Decryption{
				Provider: DecryptionProviderSOPS,
			},
			wantErr: false,
		},
		{
			name: "non-existing Decryption Secret",
			decryption: &kustomizev1.Decryption{
				Provider: DecryptionProviderSOPS,
				SecretRef: &meta.LocalObjectReference{
					Name: "does-not-exist",
				},
			},
			wantErr: true,
		},
		{
			name: "unimplemented Decryption Provider",
			decryption: &kustomizev1.Decryption{
				Provider: "not-supported",
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			cb := fake.NewClientBuilder()
			if tt.secret != nil {
				cb.WithObjects(tt.secret)
			}
			kustomization := kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      provider + "-" + tt.name,
					Namespace: provider,
				},
				Spec: kustomizev1.KustomizationSpec{
					Interval:   metav1.Duration{Duration: 2 * time.Minute},
					Path:       "./",
					Decryption: tt.decryption,
				},
			}

			d, cleanup, err := New(cb.Build(), &kustomization)
			g.Expect(err).ToNot(HaveOccurred())
			t.Cleanup(cleanup)

			match := Succeed()
			if tt.wantErr {
				match = HaveOccurred()
			}
			g.Expect(d.ImportKeys(context.TODO())).To(match)

			if tt.inspectFunc != nil {
				tt.inspectFunc(g, d)
			}
		})
	}
}

func TestDecryptor_SetAuthOptions(t *testing.T) {
	t.Run("nil decryption settings", func(t *testing.T) {
		g := NewWithT(t)

		d := &Decryptor{
			kustomization: &kustomizev1.Kustomization{},
		}

		d.SetAuthOptions(context.Background())

		g.Expect(d.awsCredentialsProvider).To(BeNil())
		g.Expect(d.azureTokenCredential).To(BeNil())
		g.Expect(d.gcpTokenSource).To(BeNil())
	})

	t.Run("non-sops provider", func(t *testing.T) {
		g := NewWithT(t)

		d := &Decryptor{
			kustomization: &kustomizev1.Kustomization{
				Spec: kustomizev1.KustomizationSpec{
					Decryption: &kustomizev1.Decryption{},
				},
			},
		}

		d.SetAuthOptions(context.Background())

		g.Expect(d.awsCredentialsProvider).To(BeNil())
		g.Expect(d.azureTokenCredential).To(BeNil())
		g.Expect(d.gcpTokenSource).To(BeNil())
	})

	t.Run("sops provider", func(t *testing.T) {
		g := NewWithT(t)

		d := &Decryptor{
			kustomization: &kustomizev1.Kustomization{
				Spec: kustomizev1.KustomizationSpec{
					Decryption: &kustomizev1.Decryption{
						Provider: DecryptionProviderSOPS,
					},
				},
			},
		}

		d.SetAuthOptions(context.Background())

		g.Expect(d.awsCredentialsProvider).NotTo(BeNil())
		g.Expect(d.azureTokenCredential).NotTo(BeNil())
		g.Expect(d.gcpTokenSource).NotTo(BeNil())
	})
}

func TestDecryptor_SopsDecryptWithFormat(t *testing.T) {
	t.Run("decrypt INI to INI", func(t *testing.T) {
		g := NewWithT(t)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())

		kd := &Decryptor{
			checkSopsMac:  true,
			ageIdentities: age.ParsedIdentities{ageID},
		}

		format := formats.Ini
		data := []byte("[config]\nkey = value\n")
		encData, err := kd.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&age.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, data, format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(bytes.Contains(encData, sopsFormatToMarkerBytes[format])).To(BeTrue())
		g.Expect(encData).ToNot(Equal(data))

		out, err := kd.SopsDecryptWithFormat(encData, format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(out).To(Equal(data))
	})

	t.Run("decrypt JSON to YAML", func(t *testing.T) {
		g := NewWithT(t)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())

		kd := &Decryptor{
			checkSopsMac:  true,
			ageIdentities: age.ParsedIdentities{ageID},
		}

		inputFormat, outputFormat := formats.Json, formats.Yaml
		data := []byte("{\"key\": \"value\"}\n")
		encData, err := kd.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&age.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, data, inputFormat, inputFormat)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(bytes.Contains(encData, sopsFormatToMarkerBytes[inputFormat])).To(BeTrue())

		out, err := kd.SopsDecryptWithFormat(encData, inputFormat, outputFormat)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(out).To(Equal([]byte("key: value\n")))
	})

	t.Run("invalid JSON data", func(t *testing.T) {
		g := NewWithT(t)

		format := formats.Json
		data, err := (&Decryptor{}).SopsDecryptWithFormat([]byte("invalid json"), format, format)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to load encrypted JSON data"))
		g.Expect(data).To(BeNil())
	})

	t.Run("no data key", func(t *testing.T) {
		g := NewWithT(t)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())

		kd := &Decryptor{}

		format := formats.Binary
		encData, err := kd.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&age.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, []byte("foo bar"), format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(bytes.Contains(encData, sopsFormatToMarkerBytes[format])).To(BeTrue())

		data, err := kd.SopsDecryptWithFormat(encData, format, format)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("cannot get sops data key"))
		g.Expect(data).To(BeNil())
	})

	t.Run("with mac check", func(t *testing.T) {
		g := NewWithT(t)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())

		kd := &Decryptor{
			checkSopsMac:  true,
			ageIdentities: age.ParsedIdentities{ageID},
		}

		format := formats.Dotenv
		data := []byte("key=value\n")
		encData, err := kd.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&age.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, data, format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(bytes.Contains(encData, sopsFormatToMarkerBytes[format])).To(BeTrue())

		out, err := kd.SopsDecryptWithFormat(encData, format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(out).To(Equal(data))

		badMAC := regexp.MustCompile("(?m)[\r\n]+^.*sops_mac=.*$")
		badMACData := badMAC.ReplaceAll(encData, []byte("\nsops_mac=\n"))
		out, err = kd.SopsDecryptWithFormat(badMACData, format, format)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to verify sops data integrity: expected mac 'no MAC'"))
		g.Expect(out).To(BeNil())
	})
}

func TestDecryptor_DecryptResource(t *testing.T) {
	var (
		resourceFactory  = provider.NewDefaultDepProvider().GetResourceFactory()
		emptyResource, _ = resourceFactory.FromMap(map[string]interface{}{})
	)

	newSecretResource := func(namespace, name string, data map[string]interface{}) (*resource.Resource, error) {
		return resourceFactory.FromMap(map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
			"data": data,
		})
	}

	kustomization := kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "decrypt",
			Namespace: "decrypt",
		},
		Spec: kustomizev1.KustomizationSpec{
			Interval: metav1.Duration{Duration: 2 * time.Minute},
			Path:     "./",
		},
	}

	t.Run("SOPS-encrypted Secret resource", func(t *testing.T) {
		g := NewWithT(t)

		kus := kustomization.DeepCopy()
		kus.Spec.Decryption = &kustomizev1.Decryption{
			Provider: DecryptionProviderSOPS,
		}

		d, cleanup, err := New(fake.NewClientBuilder().Build(), kus)
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())
		d.ageIdentities = append(d.ageIdentities, ageID)

		secret, _ := newSecretResource("test", "secret", map[string]interface{}{
			"key": "value",
		})
		g.Expect(isSOPSEncryptedResource(secret)).To(BeFalse())

		secretData, err := secret.MarshalJSON()
		g.Expect(err).ToNot(HaveOccurred())

		encData, err := d.sopsEncryptWithFormat(sops.Metadata{
			EncryptedRegex: "^(data|stringData)$",
			KeyGroups: []sops.KeyGroup{
				{&age.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, secretData, formats.Json, formats.Json)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(secret.UnmarshalJSON(encData)).To(Succeed())
		g.Expect(isSOPSEncryptedResource(secret)).To(BeTrue())

		got, err := d.DecryptResource(secret)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).ToNot(BeNil())
		g.Expect(got.MarshalJSON()).To(Equal(secretData))
	})

	t.Run("SOPS-encrypted binary-format Secret data field", func(t *testing.T) {
		g := NewWithT(t)

		kus := kustomization.DeepCopy()
		kus.Spec.Decryption = &kustomizev1.Decryption{
			Provider: DecryptionProviderSOPS,
		}

		d, cleanup, err := New(fake.NewClientBuilder().Build(), kus)
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())
		d.ageIdentities = append(d.ageIdentities, ageID)

		plainData := []byte("[config]\napp = secret\n")
		encData, err := d.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&age.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, plainData, formats.Ini, formats.Yaml)
		g.Expect(err).ToNot(HaveOccurred())

		secret, _ := newSecretResource("test", "secret-data", map[string]interface{}{
			"file.ini": base64.StdEncoding.EncodeToString(encData),
		})
		g.Expect(isSOPSEncryptedResource(secret)).To(BeFalse())

		got, err := d.DecryptResource(secret)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).ToNot(BeNil())
		g.Expect(got.GetDataMap()).To(HaveKeyWithValue("file.ini", base64.StdEncoding.EncodeToString(plainData)))
	})

	t.Run("SOPS-encrypted YAML-format Secret data field", func(t *testing.T) {
		g := NewWithT(t)

		kus := kustomization.DeepCopy()
		kus.Spec.Decryption = &kustomizev1.Decryption{
			Provider: DecryptionProviderSOPS,
		}

		d, cleanup, err := New(fake.NewClientBuilder().Build(), kus)
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())
		d.ageIdentities = append(d.ageIdentities, ageID)

		plainData := []byte("structured:\n    data:\n        key: value\n")
		encData, err := d.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&age.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, plainData, formats.Yaml, formats.Yaml)
		g.Expect(err).ToNot(HaveOccurred())

		secret, _ := newSecretResource("test", "secret-data", map[string]interface{}{
			"key.yaml": base64.StdEncoding.EncodeToString(encData),
		})
		g.Expect(isSOPSEncryptedResource(secret)).To(BeFalse())

		got, err := d.DecryptResource(secret)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).ToNot(BeNil())
		g.Expect(got.GetDataMap()).To(HaveKeyWithValue("key.yaml", base64.StdEncoding.EncodeToString(plainData)))
	})

	t.Run("SOPS-encrypted Docker config Secret", func(t *testing.T) {
		g := NewWithT(t)

		kus := kustomization.DeepCopy()
		kus.Spec.Decryption = &kustomizev1.Decryption{
			Provider: DecryptionProviderSOPS,
		}

		d, cleanup, err := New(fake.NewClientBuilder().Build(), kus)
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())
		d.ageIdentities = append(d.ageIdentities, ageID)

		plainData := []byte(`{
	"auths": {
		"my-registry.example:5000": {
			"username": "tiger",
			"password": "pass1234",
			"email": "tiger@acme.example",
			"auth": "dGlnZXI6cGFzczEyMzQ="
		}
	}
}`)
		encData, err := d.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&age.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, plainData, formats.Json, formats.Yaml)
		g.Expect(err).ToNot(HaveOccurred())

		secret, _ := resourceFactory.FromMap(map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":      "secret",
				"namespace": "test",
			},
			"type": corev1.SecretTypeDockerConfigJson,
			"data": map[string]interface{}{
				corev1.DockerConfigJsonKey: base64.StdEncoding.EncodeToString(encData),
			},
		})
		g.Expect(isSOPSEncryptedResource(secret)).To(BeFalse())

		got, err := d.DecryptResource(secret)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).ToNot(BeNil())
		plainDataWithTrailingNewline := append(plainData, '\n') // https://github.com/getsops/sops/issues/1825
		g.Expect(got.GetDataMap()).To(HaveKeyWithValue(corev1.DockerConfigJsonKey, base64.StdEncoding.EncodeToString(plainDataWithTrailingNewline)))
	})

	t.Run("nil resource", func(t *testing.T) {
		g := NewWithT(t)

		d, cleanup, err := New(fake.NewClientBuilder().Build(), kustomization.DeepCopy())
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		got, err := d.DecryptResource(nil)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(BeNil())
	})

	t.Run("no decryption spec", func(t *testing.T) {
		g := NewWithT(t)

		d, cleanup, err := New(fake.NewClientBuilder().Build(), kustomization.DeepCopy())
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		got, err := d.DecryptResource(emptyResource.DeepCopy())
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(BeNil())
	})

	t.Run("unimplemented decryption provider", func(t *testing.T) {
		g := NewWithT(t)

		kus := kustomization.DeepCopy()
		kus.Spec.Decryption = &kustomizev1.Decryption{
			Provider: "not-supported",
		}
		d, cleanup, err := New(fake.NewClientBuilder().Build(), kus)
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		got, err := d.DecryptResource(emptyResource.DeepCopy())
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(BeNil())
	})
}

func TestDecryptor_decryptKustomizationSources(t *testing.T) {
	type file struct {
		name           string
		symlink        string
		data           []byte
		originalFormat *formats.Format
		encrypt        bool
		expectData     bool
	}
	binaryFormat := formats.Binary
	tests := []struct {
		name            string
		wordirSuffix    string
		path            string
		files           []file
		secretGenerator []kustypes.SecretArgs
		expectVisited   []string
		wantErr         error
	}{
		{
			name: "decrypt env sources",
			path: "subdir",
			files: []file{
				{name: "subdir/app.env", data: []byte("var1=value1\n"), encrypt: true, expectData: true},
				// NB: Despite the file extension representing the SOPS-encrypted JSON output
				// format, the original data is plain text, or "binary."
				{name: "subdir/combination.json", data: []byte("The safe combination is ..."), originalFormat: &binaryFormat, encrypt: true, expectData: true},
				{name: "subdir/file.txt", data: []byte("file"), encrypt: true, expectData: true},
				{name: "secret.env", data: []byte("var2=value2\n"), encrypt: true, expectData: true},
			},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							FileSources: []string{"file.txt", "combo=combination.json"},
							EnvSources:  []string{"app.env", "../secret.env"},
						},
					},
				},
			},
			expectVisited: []string{"subdir/app.env", "subdir/combination.json", "subdir/file.txt", "secret.env"},
		},
		{
			name:  "decryption error",
			files: []file{},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							EnvSources: []string{"file.txt"},
						},
					},
				},
			},
			expectVisited: []string{},
			wantErr:       &fs.PathError{Op: "lstat", Path: "file.txt", Err: fmt.Errorf("")},
		},
		{
			name: "follows relative symlink within root",
			path: "subdir",
			files: []file{
				{name: "subdir/symlink", symlink: "../otherdir/data.env"},
				{name: "otherdir/data.env", data: []byte("key=value\n"), encrypt: true, expectData: true},
			},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							EnvSources: []string{"symlink"},
						},
					},
				},
			},
			expectVisited: []string{"otherdir/data.env"},
		},
		{
			name:         "error on symlink outside root",
			wordirSuffix: "subdir",
			path:         "./",
			files: []file{
				{name: "subdir/symlink", symlink: "../otherdir/data.env"},
				{name: "otherdir/data.env", data: []byte("key=value\n"), encrypt: true, expectData: false},
			},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							EnvSources: []string{"symlink"},
						},
					},
				},
			},
			wantErr:       &fs.PathError{Op: "lstat", Path: "otherdir/data.env", Err: fmt.Errorf("")},
			expectVisited: []string{},
		},
		{
			name:         "error on reference outside root",
			wordirSuffix: "subdir",
			path:         "./",
			files: []file{
				{name: "data.env", data: []byte("key=value\n"), encrypt: true, expectData: false},
			},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							EnvSources: []string{"../data.env"},
						},
					},
				},
			},
			wantErr:       &fs.PathError{Op: "lstat", Path: "data.env", Err: fmt.Errorf("")},
			expectVisited: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tmpDir := t.TempDir()
			root := filepath.Join(tmpDir, tt.wordirSuffix)

			id, err := extage.GenerateX25519Identity()
			g.Expect(err).ToNot(HaveOccurred())
			ageIdentities := age.ParsedIdentities{id}

			d := &Decryptor{
				root:          root,
				ageIdentities: ageIdentities,
			}

			for _, f := range tt.files {
				fPath := filepath.Join(tmpDir, f.name)
				g.Expect(os.MkdirAll(filepath.Dir(fPath), 0o700)).To(Succeed())
				if f.symlink != "" {
					g.Expect(os.Symlink(f.symlink, fPath)).To(Succeed())
					continue
				}
				data := f.data
				if f.encrypt {
					var format formats.Format
					if f.originalFormat != nil {
						format = *f.originalFormat
					} else {
						format = formats.FormatForPath(f.name)
					}
					data, err = d.sopsEncryptWithFormat(sops.Metadata{
						KeyGroups: []sops.KeyGroup{
							{&age.MasterKey{Recipient: id.Recipient().String()}},
						},
					}, f.data, format, format)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(data).ToNot(Equal(f.data))
				}
				g.Expect(os.WriteFile(fPath, data, 0o600)).To(Succeed())
			}

			visited := make(map[string]struct{}, 0)
			visit := d.decryptKustomizationSources(visited)
			kus := &kustypes.Kustomization{SecretGenerator: tt.secretGenerator}

			err = visit(root, tt.path, kus)
			if tt.wantErr == nil {
				g.Expect(err).ToNot(HaveOccurred())
			} else {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err).To(BeAssignableToTypeOf(tt.wantErr))
				g.Expect(err.Error()).To(ContainSubstring(tt.wantErr.Error()))
			}

			for _, f := range tt.files {
				if f.symlink != "" {
					continue
				}

				b, err := os.ReadFile(filepath.Join(tmpDir, f.name))
				g.Expect(err).ToNot(HaveOccurred())
				if f.expectData {
					g.Expect(b).To(Equal(f.data))
				} else {
					g.Expect(b).ToNot(Equal(f.data))
				}
			}

			absVisited := make(map[string]struct{}, 0)
			for _, v := range tt.expectVisited {
				absVisited[filepath.Join(tmpDir, v)] = struct{}{}
			}
			g.Expect(visited).To(Equal(absVisited))
		})
	}
}

func TestDecryptor_decryptSopsFile(t *testing.T) {
	g := NewWithT(t)

	id, err := extage.GenerateX25519Identity()
	g.Expect(err).ToNot(HaveOccurred())
	ageIdentities := age.ParsedIdentities{id}

	type file struct {
		name       string
		symlink    string
		data       []byte
		encrypt    bool
		format     formats.Format
		expectData bool
	}
	tests := []struct {
		name          string
		ageIdentities age.ParsedIdentities
		maxFileSize   int64
		files         []file
		path          string
		format        formats.Format
		wantErr       error
	}{
		{
			name:          "decrypt dotenv file",
			ageIdentities: age.ParsedIdentities{id},
			files: []file{
				{name: "app.env", data: []byte("app=key\n"), encrypt: true, format: formats.Dotenv, expectData: true},
			},
			path:   "app.env",
			format: formats.Dotenv,
		},
		{
			name:          "decrypt YAML file",
			ageIdentities: age.ParsedIdentities{id},
			files: []file{
				{name: "app.yaml", data: []byte("app: key\n"), encrypt: true, format: formats.Yaml, expectData: true},
			},
			path:   "app.yaml",
			format: formats.Yaml,
		},
		{
			name:    "irregular file",
			files:   []file{},
			wantErr: fmt.Errorf("cannot decrypt irregular file as it has file mode type bits set"),
		},
		{
			name:        "file exceeds max size",
			maxFileSize: 5,
			files: []file{
				{name: "app.env", data: []byte("app=key\n"), encrypt: true, format: formats.Dotenv, expectData: false},
			},
			path:    "app.env",
			wantErr: fmt.Errorf("cannot decrypt file with size (972 bytes) exceeding limit (5)"),
		},
		{
			name: "wrong file format",
			files: []file{
				{name: "app.ini", data: []byte("[app]\nkey = value"), encrypt: true, format: formats.Ini, expectData: false},
			},
			path: "app.ini",
		},
		{
			name: "does not follow symlink",
			files: []file{
				{name: "link", symlink: "../"},
			},
			path:    "link",
			wantErr: fmt.Errorf("cannot decrypt irregular file as it has file mode type bits set"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tmpDir := t.TempDir()

			d := &Decryptor{
				root:          tmpDir,
				maxFileSize:   maxEncryptedFileSize,
				ageIdentities: ageIdentities,
			}
			if tt.maxFileSize != 0 {
				d.maxFileSize = tt.maxFileSize
			}

			for _, f := range tt.files {
				fPath := filepath.Join(tmpDir, f.name)
				if f.symlink != "" {
					g.Expect(os.Symlink(f.symlink, fPath)).To(Succeed())
					continue
				}
				data := f.data
				if f.encrypt {
					b, err := d.sopsEncryptWithFormat(sops.Metadata{
						KeyGroups: []sops.KeyGroup{
							{&age.MasterKey{Recipient: id.Recipient().String()}},
						},
					}, data, f.format, f.format)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(b).ToNot(Equal(f.data))
					data = b
				}
				g.Expect(os.MkdirAll(filepath.Dir(fPath), 0o700)).To(Succeed())
				g.Expect(os.WriteFile(fPath, data, 0o600)).To(Succeed())
			}

			path := filepath.Join(tmpDir, tt.path)
			err := d.sopsDecryptFile(path, tt.format, tt.format)
			if tt.wantErr != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err).To(BeAssignableToTypeOf(tt.wantErr))
				g.Expect(err.Error()).To(ContainSubstring(tt.wantErr.Error()))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}
			for _, f := range tt.files {
				if f.symlink != "" {
					continue
				}

				b, err := os.ReadFile(filepath.Join(tmpDir, f.name))
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(bytes.Equal(f.data, b)).To(Equal(f.expectData))
			}
		})
	}
}

func TestDecryptor_secureLoadKustomizationFile(t *testing.T) {
	kusType := kustypes.TypeMeta{
		APIVersion: kustypes.KustomizationVersion,
		Kind:       kustypes.KustomizationKind,
	}
	type file struct {
		name    string
		symlink string
		data    []byte
	}
	tests := []struct {
		name       string
		rootSuffix string
		files      []file
		path       string
		want       *kustypes.Kustomization
		wantErr    error
	}{
		{
			name: "loads default kustomization file",
			files: []file{
				{name: konfig.DefaultKustomizationFileName(), data: []byte("resources:\n- resource.yaml")},
			},
			path: "./",
			want: &kustypes.Kustomization{
				TypeMeta:  kusType,
				Resources: []string{"resource.yaml"},
			},
		},
		{
			name: "loads recognized kustomization file",
			files: []file{
				{name: konfig.RecognizedKustomizationFileNames()[1], data: []byte("resources:\n- resource.yaml")},
			},
			path: "./",
			want: &kustypes.Kustomization{
				TypeMeta:  kusType,
				Resources: []string{"resource.yaml"},
			},
		},
		{
			name: "error on ambitious file match",
			files: []file{
				{name: konfig.RecognizedKustomizationFileNames()[0], data: []byte("resources:\n- resource.yaml")},
				{name: konfig.RecognizedKustomizationFileNames()[1], data: []byte("resources:\n- resource.yaml")},
			},
			path:    "./",
			wantErr: fmt.Errorf("found multiple kustomization files"),
		},
		{
			name:    "error on no file found",
			files:   []file{},
			path:    "./",
			wantErr: fmt.Errorf("no kustomization file found"),
		},
		{
			name:       "error on symlink outside root",
			rootSuffix: "subdir",
			files: []file{
				{name: konfig.DefaultKustomizationFileName(), data: []byte("resources:\n- resource.yaml")},
				{name: "subdir/" + konfig.DefaultKustomizationFileName(), symlink: "../kustomization.yaml"},
			},
			wantErr: fmt.Errorf("no kustomization file found"),
		},
		{
			name: "error on invalid file",
			files: []file{
				{name: konfig.DefaultKustomizationFileName(), data: []byte("resources")},
			},
			wantErr: fmt.Errorf("failed to unmarshal kustomization file"),
		},
		{
			name:    "error on absolute path",
			path:    "/absolute/",
			wantErr: fmt.Errorf("path '/absolute/' must be relative"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tmpDir := t.TempDir()
			for _, f := range tt.files {
				fPath := filepath.Join(tmpDir, f.name)
				if f.symlink != "" {
					g.Expect(os.Symlink(f.symlink, fPath))
					continue
				}
				g.Expect(os.MkdirAll(filepath.Dir(fPath), 0o700)).To(Succeed())
				g.Expect(os.WriteFile(fPath, f.data, 0o600)).To(Succeed())
			}

			root := filepath.Join(tmpDir, tt.rootSuffix)
			got, err := secureLoadKustomizationFile(root, tt.path)
			if wantErr := tt.wantErr; wantErr != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(wantErr.Error()))
				g.Expect(got).To(BeNil())
				return
			}

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestDecryptor_recurseKustomizationFiles(t *testing.T) {
	type kusNode struct {
		path          string
		symlink       string
		resources     []string
		visitErr      error
		visited       int
		expectVisited int
		expectCached  bool
	}
	tests := []struct {
		name         string
		wordirSuffix string
		path         string
		nodes        []*kusNode
		wantErr      error
		wantErrStr   string
	}{
		{
			name:         "recurse on resources",
			wordirSuffix: "foo",
			path:         "bar",
			nodes: []*kusNode{
				{
					path:          "foo/bar/kustomization.yaml",
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "foo/baz/kustomization.yaml",
					resources:     []string{"<tmpdir>/foo/bar/baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "foo/bar/baz/kustomization.yaml",
					resources:     []string{},
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name:         "recursive loop",
			wordirSuffix: "foo",
			path:         "bar",
			nodes: []*kusNode{
				{
					path:          "foo/bar/kustomization.yaml",
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "foo/baz/kustomization.yaml",
					resources:     []string{"../foobar"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "foo/foobar/kustomization.yaml",
					resources:     []string{"../bar"},
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name: "absolute symlink",
			path: "bar",
			nodes: []*kusNode{
				{
					path:          "bar/baz/kustomization.yaml",
					resources:     []string{"../bar/absolute"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:    "bar/absolute",
					symlink: "<tmpdir>/bar/foo/",
				},
				{
					path:          "bar/foo/kustomization.yaml",
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name: "relative symlink",
			path: "bar",
			nodes: []*kusNode{
				{
					path:          "bar/baz/kustomization.yaml",
					resources:     []string{"../bar/relative"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:    "bar/relative",
					symlink: "../foo/",
				},
				{
					path:          "bar/foo/kustomization.yaml",
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name: "recognized kustomization names",
			path: "./",
			nodes: []*kusNode{
				{
					path:          konfig.RecognizedKustomizationFileNames()[1],
					resources:     []string{"bar"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          filepath.Join("bar", konfig.RecognizedKustomizationFileNames()[0]),
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          filepath.Join("baz", konfig.RecognizedKustomizationFileNames()[2]),
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name:       "path does not exist",
			path:       "./invalid",
			wantErr:    &errRecurseIgnore{Err: fs.ErrNotExist},
			wantErrStr: "lstat invalid",
		},
		{
			name: "path is not a directory",
			path: "./file.txt",
			nodes: []*kusNode{
				{
					path: "file.txt",
				},
			},
			wantErr:    &errRecurseIgnore{Err: fmt.Errorf("not a directory")},
			wantErrStr: "not a directory",
		},
		{
			name: "recurse error is returned",
			path: "/foo",
			nodes: []*kusNode{
				{
					path:          "foo/kustomization.yaml",
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "baz/wrongfile.yaml",
					expectVisited: 0,
					expectCached:  false,
				},
			},
			wantErr: fmt.Errorf("no kustomization file found"),
		},
		{
			name: "recurse ignores errRecurseIgnore",
			path: "/foo",
			nodes: []*kusNode{
				{
					path:          "foo/kustomization.yaml",
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "baz",
					expectVisited: 0,
					expectCached:  false,
				},
			},
		},
		{
			name: "remote build references are ignored",
			path: "/foo",
			nodes: []*kusNode{
				{
					path: "foo/kustomization.yaml",
					resources: []string{
						"../baz",
						"https://github.com/kubernetes-sigs/kustomize//examples/multibases/dev/?ref=v1.0.6",
					},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path: "baz/kustomization.yaml",
					resources: []string{
						"github.com/Liujingfang1/mysql?ref=test",
					},
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name: "visit error is returned",
			path: "/",
			nodes: []*kusNode{
				{
					path: "kustomization.yaml",
					resources: []string{
						"baz",
					},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "baz/kustomization.yaml",
					visitErr:      fmt.Errorf("visit error"),
					expectVisited: 1,
					expectCached:  true,
				},
			},
			wantErr:    fmt.Errorf("visit error"),
			wantErrStr: "visit error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tmpDir := t.TempDir()

			for _, n := range tt.nodes {
				path := filepath.Join(tmpDir, n.path)
				if n.symlink != "" {
					g.Expect(os.Symlink(strings.Replace(n.symlink, "<tmpdir>", tmpDir, 1), path)).To(Succeed())
					return
				}
				kus := kustypes.Kustomization{
					TypeMeta: kustypes.TypeMeta{
						APIVersion: kustypes.KustomizationVersion,
						Kind:       kustypes.KustomizationKind,
					},
				}
				for _, res := range n.resources {
					res = strings.Replace(res, "<tmpdir>", tmpDir, 1)
					kus.Resources = append(kus.Resources, res)
				}
				b, err := yaml.Marshal(kus)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(os.MkdirAll(filepath.Dir(path), 0o700)).To(Succeed())
				g.Expect(os.WriteFile(path, b, 0o600))
			}

			visit := func(root, path string, kus *kustypes.Kustomization) error {
				if filepath.IsAbs(path) {
					path = stripRoot(root, path)
				}
				for _, n := range tt.nodes {
					if dir := filepath.Dir(n.path); filepath.Join(tt.wordirSuffix, path) != dir {
						continue
					}
					n.visited++
					if n.visitErr != nil {
						return n.visitErr
					}
				}
				return nil
			}

			visited := make(map[string]struct{}, 0)
			err := recurseKustomizationFiles(filepath.Join(tmpDir, tt.wordirSuffix), tt.path, visit, visited)
			if tt.wantErr != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err).To(BeAssignableToTypeOf(tt.wantErr))
				if tt.wantErrStr != "" {
					g.Expect(err.Error()).To(ContainSubstring(tt.wantErrStr))
				}
				return
			}

			g.Expect(err).ToNot(HaveOccurred())
			for _, n := range tt.nodes {
				g.Expect(n.visited).To(Equal(n.expectVisited), n.path)

				haveCache := HaveKey(filepath.Dir(filepath.Join(tmpDir, n.path)))
				if n.expectCached {
					g.Expect(visited).To(haveCache)
				} else {
					g.Expect(visited).ToNot(haveCache)
				}
			}
		})
	}
}

func TestDecryptor_isSOPSEncryptedResource(t *testing.T) {
	g := NewWithT(t)

	resourceFactory := provider.NewDefaultDepProvider().GetResourceFactory()
	encrypted, _ := resourceFactory.FromMap(map[string]interface{}{
		"sops": map[string]string{
			"mac": "some mac value",
		},
	})
	empty, _ := resourceFactory.FromMap(map[string]interface{}{})

	g.Expect(isSOPSEncryptedResource(encrypted)).To(BeTrue())
	g.Expect(isSOPSEncryptedResource(empty)).To(BeFalse())
}

func TestDecryptor_secureAbsPath(t *testing.T) {
	tests := []struct {
		name    string
		root    string
		path    string
		wantAbs string
		wantRel string
		wantErr bool
	}{
		{
			name:    "absolute to root",
			root:    "/wordir/",
			path:    "/wordir/foo/",
			wantAbs: "/wordir/foo",
			wantRel: "foo",
		},
		{
			name:    "relative to root",
			root:    "/wordir",
			path:    "./foo",
			wantAbs: "/wordir/foo",
			wantRel: "foo",
		},
		{
			name:    "illegal traverse",
			root:    "/wordir/foo",
			path:    "../../bar",
			wantAbs: "/wordir/foo/bar",
			wantRel: "bar",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			gotAbs, gotRel, err := securePaths(tt.root, tt.path)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				g.Expect(gotAbs).To(BeEmpty())
				g.Expect(gotRel).To(BeEmpty())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(gotAbs).To(Equal(tt.wantAbs))
			g.Expect(gotRel).To(Equal(tt.wantRel))
		})
	}
}

func TestDecryptor_formatForPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want formats.Format
	}{
		{
			name: "docker config",
			path: corev1.DockerConfigJsonKey,
			want: formats.Json,
		},
		{
			name: "fallback",
			path: "foo.yaml",
			want: formats.Yaml,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			g.Expect(formatForPath(tt.path)).To(Equal(tt.want))
		})
	}
}

func TestDecryptor_detectFormatFromMarkerBytes(t *testing.T) {
	tests := []struct {
		name string
		b    []byte
		want formats.Format
	}{
		{
			name: "detects format",
			b:    bytes.Join([][]byte{[]byte("random other bytes"), sopsFormatToMarkerBytes[formats.Yaml], []byte("more random bytes")}, []byte(" ")),
			want: formats.Yaml,
		},
		{
			name: "returns unsupported format",
			b:    []byte("no marker bytes present"),
			want: unsupportedFormat,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectFormatFromMarkerBytes(tt.b); got != tt.want {
				t.Errorf("detectFormatFromMarkerBytes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSafeDecrypt(t *testing.T) {
	for _, tt := range []struct {
		name        string
		mac         string
		err         string
		expectedMac string
		expectedErr string
	}{
		{
			name:        "no error",
			mac:         "some mac",
			expectedMac: "some mac",
		},
		{
			name:        "only prefix",
			err:         "Input string was not in a correct format",
			expectedErr: "Input string was not in a correct format",
		},
		{
			name:        "only suffix",
			err:         "The value does not match sops' data format",
			expectedErr: "The value does not match sops' data format",
		},
		{
			name:        "redacted value",
			err:         "Input string 1234567897 does not match sops' data format",
			expectedErr: "Input string <redacted> does not match sops' data format",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			var err error
			if tt.err != "" {
				err = errors.New(tt.err)
			}

			mac, err := safeDecrypt(tt.mac, err)

			g.Expect(mac).To(Equal(tt.expectedMac))

			if tt.expectedErr == "" {
				g.Expect(err).To(Not(HaveOccurred()))
			} else {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(Equal(tt.expectedErr))
			}
		})
	}
}

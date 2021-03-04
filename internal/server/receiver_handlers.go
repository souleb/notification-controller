/*
Copyright 2020 The Flux authors

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

package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"net/http"
	"net/url"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/google/go-github/v32/github"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fluxcd/notification-controller/api/v1beta1"
)

func (s *ReceiverServer) handlePayload() func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		digest := url.PathEscape(strings.TrimLeft(r.RequestURI, "/hook/"))

		s.logger.Info("handling request", "digest", digest)

		var allReceivers v1beta1.ReceiverList
		err := s.kubeClient.List(ctx, &allReceivers)
		if err != nil {
			s.logger.Error(err, "unable to list receivers")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		receivers := make([]v1beta1.Receiver, 0)
		for _, receiver := range allReceivers.Items {
			if !receiver.Spec.Suspend &&
				apimeta.IsStatusConditionTrue(receiver.Status.Conditions, meta.ReadyCondition) &&
				receiver.Status.URL == fmt.Sprintf("/hook/%s", digest) {
				receivers = append(receivers, receiver)
			}
		}

		if len(receivers) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		withErrors := false
		for _, receiver := range receivers {
			if err := s.validate(ctx, receiver, r); err != nil {
				s.logger.Error(err, "unable to validate payload",
					"receiver", receiver.Name)
				withErrors = true
				continue
			}

			s.logger.Info("found matching receiver", "receiver", receiver.Name)
			for _, resource := range receiver.Spec.Resources {
				if err := s.annotate(ctx, resource, receiver.Namespace); err != nil {
					s.logger.Error(err, "unable to annotate resource",
						"receiver", receiver.Name)
					withErrors = true
				} else {
					s.logger.Info("resource annotated", "receiver", receiver.Name,
						"resource", resource.Name)
				}
			}
		}

		if withErrors {
			w.WriteHeader(http.StatusBadRequest)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}
}

func (s *ReceiverServer) validate(ctx context.Context, receiver v1beta1.Receiver, r *http.Request) error {
	token, err := s.token(ctx, receiver)
	if err != nil {
		return fmt.Errorf("unable to read token, error: %w", err)
	}

	switch receiver.Spec.Type {
	case v1beta1.GenericReceiver:
		return nil
	case v1beta1.GenericHMACReceiver:
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return fmt.Errorf("unable to read request body: %s", err)
		}

		err = github.ValidateSignature(r.Header.Get("X-Signature"), b, []byte(token))
		if err != nil {
			return fmt.Errorf("unable to validate signature: %s", err)
		}

		s.logger.Info(
			"handling event from generic-hmac wehbook",
			"receiver", receiver.Name)
		return nil
	case v1beta1.GitHubReceiver:
		payload, err := github.ValidatePayload(r, []byte(token))
		if err != nil {
			return fmt.Errorf("the GitHub signature header is invalid, err: %w", err)
		}

		if _, err := github.ParseWebHook(github.WebHookType(r), payload); err != nil {
			return fmt.Errorf("unable to parse GitHub payload, err: %w", err)
		}

		event := github.WebHookType(r)

		if len(receiver.Spec.Events) > 0 {
			allowed := false
			for _, e := range receiver.Spec.Events {
				if strings.ToLower(event) == strings.ToLower(e) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("the GitHub event '%s' is not authorised", event)
			}
		}

		s.logger.Info("handling GitHub event: "+event, "receiver", receiver.Name)
		return nil
	case v1beta1.GitLabReceiver:
		if r.Header.Get("X-Gitlab-Token") != token {
			return fmt.Errorf("the X-Gitlab-Token header value does not match the receiver token")
		}

		event := r.Header.Get("X-Gitlab-Event")
		if len(receiver.Spec.Events) > 0 {
			allowed := false
			for _, e := range receiver.Spec.Events {
				if strings.ToLower(event) == strings.ToLower(e) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("the GitLab event '%s' is not authorised", event)
			}
		}

		s.logger.Info("handling GitLab event: "+event, "receiver", receiver.Name)
		return nil
	case v1beta1.BitbucketReceiver:
		_, err := github.ValidatePayload(r, []byte(token))
		if err != nil {
			return fmt.Errorf("the Bitbucket server signature header is invalid, err: %w", err)
		}

		event := r.Header.Get("X-Event-Key")

		if len(receiver.Spec.Events) > 0 {
			allowed := false
			for _, e := range receiver.Spec.Events {
				if strings.ToLower(event) == strings.ToLower(e) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("the Bitbucket server event '%s' is not authorised", event)
			}
		}

		s.logger.Info("handling Bitbucket server event: "+event, "receiver", receiver.Name)
		return nil
	case v1beta1.QuayReceiver:
		type payload struct {
			DockerUrl   string   `json:"docker_url"`
			UpdatedTags []string `json:"updated_tags"`
		}

		var p payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			return fmt.Errorf("cannot decode Quay webhook payload")
		}

		s.logger.Info(
			fmt.Sprintf("handling event from %s", p.DockerUrl),
			"receiver", receiver.Name)
		return nil
	case v1beta1.HarborReceiver:
		if r.Header.Get("Authorization") != token {
			return fmt.Errorf("the Harbor Authorization header value does not match the receiver token")
		}

		s.logger.Info("handling Harbor event", "receiver", receiver.Name)
		return nil
	case v1beta1.DockerHubReceiver:
		type payload struct {
			PushData struct {
				Tag string `json:"tag"`
			} `json:"push_data"`
			Repository struct {
				URL string `json:"repo_url"`
			} `json:"repository"`
		}
		var p payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			return fmt.Errorf("cannot decode DockerHub webhook payload")
		}

		s.logger.Info(
			fmt.Sprintf("handling event from %s for tag %s", p.Repository.URL, p.PushData.Tag),
			"receiver", receiver.Name)
		return nil
	case v1beta1.GCRReceiver:
		const (
			insert     = "insert"
			tokenIndex = len("Bearer ")
		)

		type data struct {
			Action string `json:"action"`
			Digest string `json:"digest"`
			Tag    string `json:"tag"`
		}

		type payload struct {
			Message struct {
				Data         string    `json:"data"`
				MessageID    string    `json:"messageId"`
				PublishTime  time.Time `json:"publishTime"`
				Subscription string    `json:"subscription"`
			} `json:"message"`
		}

		err := authenticateGCRRequest(&http.Client{}, r.Header.Get("Authorization"), tokenIndex)
		if err != nil {
			return fmt.Errorf("cannot authenticate GCR request: %s", err)
		}

		var p payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			return fmt.Errorf("cannot decode GCR webhook payload")
		}

		raw, _ := base64.StdEncoding.DecodeString(p.Message.Data)

		var d data
		err = json.Unmarshal(raw, &d)
		if err != nil {
			return fmt.Errorf("cannot decode GCR webhook body")
		}

		if strings.ToLower(d.Action) != insert {
			s.logger.Info("action is not an insert, moving on")
			return nil
		}

		s.logger.Info(
			fmt.Sprintf("handling event from %s for tag %s", d.Digest, d.Tag),
			"receiver", receiver.Name)
		return nil
	case v1beta1.NexusReceiver:
		signature := r.Header.Get("X-Nexus-Webhook-Signature")
		if len(signature) == 0 {
			return fmt.Errorf("Signature is missing from header")
		}

		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return fmt.Errorf("cannot read payload. error: %s", err)
		}

		if !verifyHmacSignature([]byte(token), signature, b) {
			return fmt.Errorf("invalid nexus signature")
		}
		type payload struct {
			Action         string `json:"action"`
			RepositoryName string `json:"repositoryName"`
		}
		var p payload

		if err := json.Unmarshal(b, &p); err != nil {
			return fmt.Errorf("cannot decode Nexus webhook payload: %s", err)
		}

		s.logger.Info(
			fmt.Sprintf("handling event from %s", p.RepositoryName),
			"receiver", receiver.Name)
		return nil
	}

	return fmt.Errorf("recevier type '%s' not supported", receiver.Spec.Type)
}

func (s *ReceiverServer) token(ctx context.Context, receiver v1beta1.Receiver) (string, error) {
	token := ""
	secretName := types.NamespacedName{
		Namespace: receiver.GetNamespace(),
		Name:      receiver.Spec.SecretRef.Name,
	}

	var secret corev1.Secret
	err := s.kubeClient.Get(ctx, secretName, &secret)
	if err != nil {
		return "", fmt.Errorf("unable to read token from secret '%s' error: %w", secretName, err)
	}

	if val, ok := secret.Data["token"]; ok {
		token = string(val)
	} else {
		return "", fmt.Errorf("invalid '%s' secret data: required field 'token'", secretName)
	}

	return token, nil
}

func (s *ReceiverServer) annotate(ctx context.Context, resource v1beta1.CrossNamespaceObjectReference, defaultNamespace string) error {
	namespace := defaultNamespace
	if resource.Namespace != "" {
		namespace = resource.Namespace
	}
	objectKey := client.ObjectKey{
		Namespace: namespace,
		Name:      resource.Name,
	}

	apiVersionMap := map[string]string{
		"Bucket":          "source.toolkit.fluxcd.io/v1beta1",
		"HelmRepository":  "source.toolkit.fluxcd.io/v1beta1",
		"GitRepository":   "source.toolkit.fluxcd.io/v1beta1",
		"ImageRepository": "image.toolkit.fluxcd.io/v1alpha1",
	}

	apiVersion := resource.APIVersion
	if apiVersion == "" {
		if apiVersionMap[resource.Kind] == "" {
			return fmt.Errorf("apiVersion must be specified for kind '%s'", resource.Kind)
		}
		apiVersion = apiVersionMap[resource.Kind]
	}

	group, version := getGroupVersion(apiVersion)

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   group,
		Kind:    resource.Kind,
		Version: version,
	})

	if err := s.kubeClient.Get(ctx, objectKey, u); err != nil {
		return fmt.Errorf("unable to read %s '%s' error: %w", resource.Kind, objectKey, err)
	}

	sourceAnnotations := u.GetAnnotations()
	if sourceAnnotations == nil {
		sourceAnnotations = make(map[string]string)
	}
	sourceAnnotations[meta.ReconcileRequestAnnotation] = metav1.Now().String()
	u.SetAnnotations(sourceAnnotations)
	if err := s.kubeClient.Update(ctx, u); err != nil {
		return fmt.Errorf("unable to annotate %s '%s' error: %w", resource.Kind, objectKey, err)
	}

	return nil
}

func authenticateGCRRequest(c *http.Client, bearer string, tokenIndex int) (err error) {
	type auth struct {
		Aud string `json:"aud"`
	}

	if len(bearer) < tokenIndex {
		return fmt.Errorf("Authorization header is missing or malformed: %v", bearer)
	}

	token := bearer[tokenIndex:]
	url := fmt.Sprintf("https://oauth2.googleapis.com/tokeninfo?id_token=%s", token)

	resp, err := c.Get(url)
	if err != nil {
		return fmt.Errorf("Cannot verify authenticity of payload: %w", err)
	}

	var p auth
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return fmt.Errorf("Cannot decode auth payload: %w", err)
	}

	return nil
}

func verifyHmacSignature(key []byte, signature string, payload []byte) bool {
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(payload)
	expectedMAC := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expectedMAC))
}

func getGroupVersion(s string) (string, string) {
	slice := strings.Split(s, "/")
	if len(slice) == 1 {
		return "", slice[0]
	}

	return slice[0], slice[1]
}

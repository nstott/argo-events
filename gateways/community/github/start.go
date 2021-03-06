/*
Copyright 2018 KompiTech GmbH

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

package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/argoproj/argo-events/common"
	"github.com/argoproj/argo-events/gateways"
	gwcommon "github.com/argoproj/argo-events/gateways/common"
	"github.com/argoproj/argo-events/store"
	gh "github.com/google/go-github/github"
	corev1 "k8s.io/api/core/v1"
)

const (
	LabelGithubConfig = "config"
	LabelGithubClient = "client"
	LabelWebhook      = "hook"
)

const (
	GithubSignatureHeader = "x-hub-signature"
	GithubEventHeader     = "x-github-event"
	GithubDeliveryHeader  = "x-github-delivery"
)

var (
	helper = gwcommon.NewWebhookHelper()
)

func init() {
	go gwcommon.InitRouteChannels(helper)
}

// getCredentials for github
func (ese *GithubEventSourceExecutor) getCredentials(gs *corev1.SecretKeySelector) (*cred, error) {
	token, err := store.GetSecrets(ese.Clientset, ese.Namespace, gs.Name, gs.Key)
	if err != nil {
		return nil, err
	}
	return &cred{
		secret: token,
	}, nil
}

func (ese *GithubEventSourceExecutor) PostActivate(rc *gwcommon.RouteConfig) error {
	gc := rc.Configs[LabelGithubConfig].(*GithubConfig)

	c, err := ese.getCredentials(gc.APIToken)
	if err != nil {
		return fmt.Errorf("failed to rtrieve github credentials. err: %+v", err)
	}

	PATTransport := TokenAuthTransport{
		Token: c.secret,
	}

	hookConfig := map[string]interface{}{
		"url": &gc.URL,
	}

	if gc.ContentType != "" {
		hookConfig["content_type"] = gc.ContentType
	}

	if gc.Insecure {
		hookConfig["insecure_ssl"] = "1"
	} else {
		hookConfig["insecure_ssl"] = "0"
	}

	if gc.WebHookSecret != nil {
		sc, err := ese.getCredentials(gc.WebHookSecret)
		if err != nil {
			return fmt.Errorf("failed to retrieve webhook secret. err: %+v", err)
		}
		hookConfig["secret"] = sc.secret
	}

	hookSetup := &gh.Hook{
		Events: gc.Events,
		Active: gh.Bool(gc.Active),
		Config: hookConfig,
	}

	client := gh.NewClient(PATTransport.Client())
	rc.Configs[LabelGithubClient] = client

	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)

	hook, _, err := client.Repositories.CreateHook(ctx, gc.Owner, gc.Repository, hookSetup)
	if err != nil {
		return fmt.Errorf("failed to create webhook. err: %+v", err)
	}
	rc.Configs[LabelWebhook] = hook

	ese.Log.Info().Str("event-source-name", rc.EventSource.Name).Interface("hook-id", *hook.ID).Msg("github hook created")
	return nil
}

func PostStop(rc *gwcommon.RouteConfig) error {
	gc := rc.Configs[LabelGithubConfig].(*GithubConfig)
	client := rc.Configs[LabelGithubClient].(*gh.Client)
	hook := rc.Configs[LabelWebhook].(*gh.Hook)

	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := client.Repositories.DeleteHook(ctx, gc.Owner, gc.Repository, *hook.ID); err != nil {
		rc.Log.Error().Err(err).Str("event-source-name", rc.EventSource.Name).Msg("failed to delete github hook")
		return err
	}
	rc.Log.Info().Str("event-source-name", rc.EventSource.Name).Interface("hook-id", *hook.ID).Msg("github hook deleted")
	return nil
}

// StartEventSource starts an event source
func (ese *GithubEventSourceExecutor) StartEventSource(eventSource *gateways.EventSource, eventStream gateways.Eventing_StartEventSourceServer) error {
	defer gateways.Recover(eventSource.Name)

	ese.Log.Info().Str("event-source-name", eventSource.Name).Msg("operating on event source")
	gc, err := parseEventSource(eventSource.Data)
	if err != nil {
		return fmt.Errorf("%s, err: %+v", gateways.ErrEventSourceParseFailed, err)
	}

	return gwcommon.ProcessRoute(&gwcommon.RouteConfig{
		Webhook: &gwcommon.Webhook{
			Port:     gc.Port,
			Endpoint: gwcommon.FormatWebhookEndpoint(gc.Endpoint),
		},
		Configs: map[string]interface{}{
			LabelGithubConfig: gc,
		},
		Log:                ese.Log,
		EventSource:        eventSource,
		PostActivate:       ese.PostActivate,
		PostStop:           PostStop,
		RouteActiveHandler: RouteActiveHandler,
		StartCh:            make(chan struct{}),
	}, helper, eventStream)
}

func signBody(secret, body []byte) []byte {
	computed := hmac.New(sha1.New, secret)
	computed.Write(body)
	return []byte(computed.Sum(nil))
}

func verifySignature(secret []byte, signature string, body []byte) bool {
	const signaturePrefix = "sha1="
	const signatureLength = 45

	if len(signature) != signatureLength || !strings.HasPrefix(signature, signaturePrefix) {
		return false
	}

	actual := make([]byte, 20)
	hex.Decode(actual, []byte(signature[5:]))

	return hmac.Equal(signBody(secret, body), actual)
}

func validatePayload(secret []byte, headers http.Header, body []byte) error {
	signature := headers.Get(GithubSignatureHeader)
	if len(signature) == 0 {
		return errors.New("no x-hub-signature header found")
	}

	if event := headers.Get(GithubEventHeader); len(event) == 0 {
		return errors.New("no x-github-event header found")
	}

	if id := headers.Get(GithubDeliveryHeader); len(id) == 0 {
		return errors.New("no x-github-delivery header found")
	}

	if !verifySignature(secret, signature, body) {
		return errors.New("invalid signature")
	}
	return nil
}

// routeActiveHandler handles new route
func RouteActiveHandler(writer http.ResponseWriter, request *http.Request, rc *gwcommon.RouteConfig) {
	var response string

	logger := rc.Log.With().Str("event-source", rc.EventSource.Name).Str("endpoint", rc.Webhook.Endpoint).
		Str("port", rc.Webhook.Port).
		Str("http-method", request.Method).Logger()
	logger.Info().Msg("request received")

	if !helper.ActiveEndpoints[rc.Webhook.Endpoint].Active {
		response = fmt.Sprintf("the route: endpoint %s and method %s is deactived", rc.Webhook.Endpoint, rc.Webhook.Method)
		logger.Info().Msg("endpoint is not active")
		common.SendErrorResponse(writer, response)
		return
	}

	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		logger.Error().Err(err).Msg("failed to parse request body")
		common.SendErrorResponse(writer, fmt.Sprintf("failed to parse request. err: %+v", err))
		return
	}

	hook := rc.Configs[LabelWebhook].(*gh.Hook)
	if secret, ok := hook.Config["secret"]; ok {
		if err := validatePayload([]byte(secret.(string)), request.Header, body); err != nil {
			logger.Error().Err(err).Msg("request is not valid event notification")
			common.SendErrorResponse(writer, fmt.Sprintf("invalid event notification"))
			return
		}
	}

	helper.ActiveEndpoints[rc.Webhook.Endpoint].DataCh <- body
	response = "request successfully processed"
	logger.Info().Msg(response)
	common.SendSuccessResponse(writer, response)
}

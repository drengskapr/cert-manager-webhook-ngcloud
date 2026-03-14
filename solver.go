package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"cert-manager-webhook-ngcloud/ngcloud"
	"github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

type NgcloudSolver struct {
	kubeClient kubernetes.Interface
}

type NgcloudSolverConfig struct {
	ZoneUID        string                   `json:"zoneUID"`
	TokenSecretRef corev1.SecretKeySelector `json:"tokenSecretRef"`
}

func (s *NgcloudSolver) Name() string {
	return "ngcloud"
}

func (s *NgcloudSolver) Initialize(kubeClientConfig *rest.Config, stopCh <-chan struct{}) error {
	cl, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return err
	}
	s.kubeClient = cl
	klog.Info("NgcloudSolver initialized")
	return nil
}

func (s *NgcloudSolver) Present(ch *v1alpha1.ChallengeRequest) error {
	klog.InfoS("Present called", "fqdn", ch.ResolvedFQDN, "zone", ch.ResolvedZone)
	cfg, token, err := s.loadConfig(ch)
	if err != nil {
		klog.ErrorS(err, "Present: failed to load config", "fqdn", ch.ResolvedFQDN)
		return err
	}
	name := recordName(ch.ResolvedFQDN, ch.ResolvedZone)
	if err := ngcloud.New(token).CreateTXTRecord(cfg.ZoneUID, name, ch.Key); err != nil {
		klog.ErrorS(err, "Present: failed to create TXT record", "fqdn", ch.ResolvedFQDN)
		return err
	}
	klog.InfoS("Present succeeded", "fqdn", ch.ResolvedFQDN)
	return nil
}

func (s *NgcloudSolver) CleanUp(ch *v1alpha1.ChallengeRequest) error {
	klog.InfoS("CleanUp called", "fqdn", ch.ResolvedFQDN, "zone", ch.ResolvedZone)
	_, token, err := s.loadConfig(ch)
	if err != nil {
		klog.ErrorS(err, "CleanUp: failed to load config", "fqdn", ch.ResolvedFQDN)
		return err
	}
	name := recordName(ch.ResolvedFQDN, ch.ResolvedZone)
	if err := ngcloud.New(token).DeleteTXTRecord(name); err != nil {
		klog.ErrorS(err, "CleanUp: failed to delete TXT record", "fqdn", ch.ResolvedFQDN)
		return err
	}
	klog.InfoS("CleanUp succeeded", "fqdn", ch.ResolvedFQDN)
	return nil
}

func (s *NgcloudSolver) loadConfig(ch *v1alpha1.ChallengeRequest) (*NgcloudSolverConfig, string, error) {
	cfg := &NgcloudSolverConfig{}
	if ch.Config != nil {
		if err := json.Unmarshal(ch.Config.Raw, cfg); err != nil {
			return nil, "", fmt.Errorf("unmarshal solver config: %w", err)
		}
	}

	if cfg.TokenSecretRef.Name == "" {
		return nil, "", fmt.Errorf("tokenSecretRef.name is required in solver config")
	}

	secret, err := s.kubeClient.CoreV1().Secrets(ch.ResourceNamespace).Get(
		context.Background(),
		cfg.TokenSecretRef.Name,
		metav1.GetOptions{},
	)
	if err != nil {
		return nil, "", fmt.Errorf("get secret %s/%s: %w", ch.ResourceNamespace, cfg.TokenSecretRef.Name, err)
	}

	token := string(secret.Data[cfg.TokenSecretRef.Key])
	if token == "" {
		return nil, "", fmt.Errorf("secret %s/%s key %q is empty", ch.ResourceNamespace, cfg.TokenSecretRef.Name, cfg.TokenSecretRef.Key)
	}

	return cfg, token, nil
}

// recordName returns the relative record name from a fully-qualified domain name.
// e.g. "_acme-challenge.example.com." with zone "example.com." → "_acme-challenge"
func recordName(fqdn, zone string) string {
	fqdn = strings.TrimSuffix(fqdn, ".")
	zone = strings.TrimSuffix(zone, ".")
	return strings.TrimSuffix(fqdn, "."+zone)
}

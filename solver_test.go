package main

import (
	"os"
	"testing"

	dns "github.com/cert-manager/cert-manager/test/acme"
	corev1 "k8s.io/api/core/v1"
)

var zone = os.Getenv("TEST_ZONE_NAME")

func TestConformance(t *testing.T) {
	if zone == "" {
		t.Skip("TEST_ZONE_NAME not set")
	}
	zoneUID := os.Getenv("TEST_ZONE_UID")
	if zoneUID == "" {
		t.Skip("TEST_ZONE_UID not set")
	}

	// Ensure the zone ends with a dot as required by the conformance framework.
	if zone[len(zone)-1] != '.' {
		zone += "."
	}

	fixture := dns.NewFixture(&NgcloudSolver{},
		dns.SetResolvedZone(zone),
		dns.SetAllowAmbientCredentials(false),
		dns.SetManifestPath("testdata/ngcloud"),
		dns.SetDNSServer("185.247.187.83:53"),
		dns.SetUseAuthoritative(false),
		dns.SetConfig(NgcloudSolverConfig{
			ZoneUID: zoneUID,
			TokenSecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "ngcloud-api-token"},
				Key:                  "token",
			},
		}),
	)
	fixture.RunConformance(t)
}

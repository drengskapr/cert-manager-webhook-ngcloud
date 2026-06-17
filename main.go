package main

import (
	"os"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	// Route the controller-runtime logger (used by ngcloud/client.go) into klog so
	// every logging path — client, solver, startup — converges on the single
	// klog/component-base pipeline the webhook framework configures via
	// --logging-format and --v.
	ctrl.SetLogger(klog.Background())

	// Register the json-rfc3339 log format before the webhook framework parses
	// flags and freezes the format registry. This adds a JSON format with
	// RFC3339Nano timestamps alongside the built-in text/json formats.
	if err := registerJSONRFC3339Format(); err != nil {
		klog.ErrorS(err, "Failed to register log format", "format", jsonRFC3339Format)
		os.Exit(1)
	}

	groupName := os.Getenv("GROUP_NAME")
	if groupName == "" {
		groupName = "acme.ngcloud.ru"
	}
	klog.InfoS("Starting cert-manager-webhook-ngcloud", "groupName", groupName)
	cmd.RunWebhookServer(groupName, &NgcloudSolver{})
}

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

	groupName := os.Getenv("GROUP_NAME")
	if groupName == "" {
		groupName = "acme.ngcloud.ru"
	}
	klog.InfoS("Starting cert-manager-webhook-ngcloud", "groupName", groupName)
	cmd.RunWebhookServer(groupName, &NgcloudSolver{})
}

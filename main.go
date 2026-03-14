package main

import (
	"os"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"
	"go.uber.org/zap/zapcore"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	opts := zap.Options{
		Development: false,
		TimeEncoder: zapcore.ISO8601TimeEncoder,
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	groupName := os.Getenv("GROUP_NAME")
	if groupName == "" {
		groupName = "acme.ngcloud.ru"
	}
	klog.InfoS("Starting cert-manager-webhook-ngcloud", "groupName", groupName)
	cmd.RunWebhookServer(groupName, &NgcloudSolver{})
}

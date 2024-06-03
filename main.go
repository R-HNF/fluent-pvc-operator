package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	fluentpvcoperatorv1alpha1 "github.com/st-tech/fluent-pvc-operator/api/v1alpha1"
	"github.com/st-tech/fluent-pvc-operator/controllers" // オリジナル
	"github.com/st-tech/fluent-pvc-operator/webhooks" // オリジナル
	//+kubebuilder:scaffold:imports
)

// やりとりするためのスキーマ定義
// セットアップのロガー生成
var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// client-go（goのk8s client）と、fluent-pvc-operatorのスキーマを登録
func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(fluentpvcoperatorv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	// controller-runtimeで起動するコマンドオプションの追加設定
	var configFile string
	flag.StringVar(&configFile, "config", "",
		"The controller will load its initial configuration from this file. "+
			"Omit this flag to use the default configuration values. "+
			"Command-line flags override configuration from this file.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// controller-runtimeのロガー設定
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	var err error
	// スキーマとconfigFileをオプション設定として読み込む
	options := ctrl.Options{Scheme: scheme}
	if configFile != "" {
		options, err = options.AndFrom(ctrl.ConfigFile().AtPath(configFile))
		if err != nil {
			setupLog.Error(err, "unable to load the config file")
			os.Exit(1)
		}
	}

	// managerの生成
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// FluentPVCReconcilerの登録
	if err = controllers.NewFluentPVCReconciler(mgr).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "fluentpvc_controller")
		os.Exit(1)
	}
	// FluentPVCBindingReconcilerの登録
	if err = controllers.NewFluentPVCBindingReconciler(mgr).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "fluentpvcbinding_controller")
		os.Exit(1)
	}

	// ビルトインに機能追加してる?
	// PVCReconcilerの登録
	if err = controllers.NewPVCReconciler(mgr).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "pvc_controller")
		os.Exit(1)
	}
	// PodReconcilerの登録
	if err = controllers.NewPodReconciler(mgr).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "pod_controller")
		os.Exit(1)
	}

	// PodWebhookの登録
	if err = webhooks.SetupPodWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Pod")
		os.Exit(1)
	}
	// FluentPVCWebhookの登録
	if err = webhooks.SetupFluentPVCWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "FluentPVC")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	// managerのヘルスチェック
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// managerのレディチェック
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error {
		// webhookw使用しているためtls/sslで通信
		dialer := &net.Dialer{Timeout: time.Second}
		// webhookのホストとポートを取得
		addrPort := fmt.Sprintf("%s:%d", mgr.GetWebhookServer().Host, mgr.GetWebhookServer().Port)
		conn, err := tls.DialWithDialer(dialer, "tcp", addrPort, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

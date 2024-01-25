package main

import (
	"context"
	"github.com/pkg/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"maroonedpods.io/pkg/maroonedpods-server"
	"maroonedpods.io/pkg/certificates/bootstrap"
	"maroonedpods.io/pkg/client"
	"maroonedpods.io/pkg/informers"
	"maroonedpods.io/pkg/util"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
)

func main() {
	defer klog.Flush()
	maroonedpodsNS := util.GetNamespace()

	maroonedpodsCli, err := client.GetMaroonedPodsClient()
	if err != nil {
		klog.Error(err.Error())
		os.Exit(1)
	}
	ctx := signals.SetupSignalHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := ctx.Done()

	secretInformer := informers.GetSecretInformer(maroonedpodsCli, maroonedpodsNS)
	go secretInformer.Run(stop)
	if !cache.WaitForCacheSync(stop, secretInformer.HasSynced) {
		os.Exit(1)
	}

	secretCertManager := bootstrap.NewFallbackCertificateManager(
		bootstrap.NewSecretCertificateManager(
			util.SecretResourceName,
			maroonedpodsNS,
			secretInformer.GetStore(),
		),
	)

	secretCertManager.Start()
	defer secretCertManager.Stop()

	maroonedpodsServer, err := maroonedpods_server.MaroonedPodsServer(maroonedpodsNS,
		util.DefaultHost,
		util.DefaultPort,
		secretCertManager,
		maroonedpodsCli,
	)
	if err != nil {
		klog.Fatalf("UploadProxy failed to initialize: %v\n", errors.WithStack(err))
	}

	err = maroonedpodsServer.Start()
	if err != nil {
		klog.Fatalf("TLS server failed: %v\n", errors.WithStack(err))
	}

}

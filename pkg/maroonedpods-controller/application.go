/*
 * This file is part of the MaroonedPods project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2023,Red Hat, Inc.
 *
 */
package maroonedpods_controller

import (
	"context"
	"fmt"
	"github.com/emicklei/go-restful/v3"
	"io/ioutil"
	k8sv1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	v14 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	maroonedpods_controller2 "maroonedpods.io/maroonedpods/pkg/maroonedpods-controller/maroonedpods-gate-controller"
	"maroonedpods.io/maroonedpods/pkg/maroonedpods-controller/leaderelectionconfig"
	"maroonedpods.io/maroonedpods/pkg/certificates/bootstrap"
	"maroonedpods.io/maroonedpods/pkg/client"
	"maroonedpods.io/maroonedpods/pkg/informers"
	"maroonedpods.io/maroonedpods/pkg/util"
	golog "log"
	"net/http"
	"os"
	"strconv"
)

type MaroonedPodsControllerApp struct {
	ctx                          context.Context
	maroonedpodsNs                        string
	host                         string
	LeaderElection               leaderelectionconfig.Configuration
	maroonedpodsCli                       client.MaroonedPodsClient
	maroonedPodsGateController            *maroonedpods_controller2.MaroonedPodsGateController
	podInformer                  cache.SharedIndexInformer
	maroonedpodsInformer                  cache.SharedIndexInformer
	readyChan                    chan bool
	enqueueAllGateControllerChan chan struct{}
	leaderElector                *leaderelection.LeaderElector
}

func Execute() {
	var err error
	var app = MaroonedPodsControllerApp{}

	app.LeaderElection = leaderelectionconfig.DefaultLeaderElectionConfiguration()
	app.readyChan = make(chan bool, 1)
	app.enqueueAllGateControllerChan = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.ctx = ctx

	webService := new(restful.WebService)
	webService.Path("/").Consumes(restful.MIME_JSON).Produces(restful.MIME_JSON)
	webService.Route(webService.GET("/leader").To(app.leaderProbe).Doc("Leader endpoint"))
	restful.Add(webService)

	nsBytes, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		panic(err)
	}
	app.maroonedpodsNs = string(nsBytes)

	host, err := os.Hostname()
	if err != nil {
		golog.Fatalf("unable to get hostname: %v", err)
	}
	app.host = host

	app.maroonedpodsCli, err = client.GetMaroonedPodsClient()
	app.podInformer = informers.GetPodInformer(app.maroonedpodsCli)
	app.maroonedpodsInformer = informers.GetMaroonedPodsInformer(app.maroonedpodsCli)

	stop := ctx.Done()

	app.initMaroonedPodsGateController(stop)

	app.Run(stop)

	klog.V(2).Infoln("MaroonedPods controller exited")

}

func (mca *MaroonedPodsControllerApp) leaderProbe(_ *restful.Request, response *restful.Response) {
	res := map[string]interface{}{}
	select {
	case _, opened := <-mca.readyChan:
		if !opened {
			res["apiserver"] = map[string]interface{}{"leader": "true"}
			if err := response.WriteHeaderAndJson(http.StatusOK, res, restful.MIME_JSON); err != nil {
				klog.Warningf("failed to return 200 OK reply: %v", err)
			}
			return
		}
	default:
	}
	res["apiserver"] = map[string]interface{}{"leader": "false"}
	if err := response.WriteHeaderAndJson(http.StatusOK, res, restful.MIME_JSON); err != nil {
		klog.Warningf("failed to return 200 OK reply: %v", err)
	}
}


func (mca *MaroonedPodsControllerApp) initMaroonedPodsGateController(stop <-chan struct{}) {
	mca.maroonedPodsGateController = maroonedpods_controller2.NewMaroonedPodsGateController(mca.maroonedpodsCli,
		mca.podInformer,
		stop,
		mca.enqueueAllGateControllerChan,
	)
}



func (mca *MaroonedPodsControllerApp) Run(stop <-chan struct{}) {
	secretInformer := informers.GetSecretInformer(mca.maroonedpodsCli, mca.maroonedpodsNs)
	go secretInformer.Run(stop)
	if !cache.WaitForCacheSync(stop, secretInformer.HasSynced) {
		os.Exit(1)
	}

	secretCertManager := bootstrap.NewFallbackCertificateManager(
		bootstrap.NewSecretCertificateManager(
			util.SecretResourceName,
			mca.maroonedpodsNs,
			secretInformer.GetStore(),
		),
	)

	secretCertManager.Start()
	defer secretCertManager.Stop()

	tlsConfig := util.SetupTLS(secretCertManager)

	go func() {
		server := http.Server{
			Addr:      fmt.Sprintf("%s:%s", util.DefaultHost, strconv.Itoa(util.DefaultPort)),
			Handler:   http.DefaultServeMux,
			TLSConfig: tlsConfig,
		}
		if err := server.ListenAndServeTLS("", ""); err != nil {
			golog.Fatal(err)
		}
	}()
	if err := mca.setupLeaderElector(); err != nil {
		golog.Fatal(err)
	}
	mca.leaderElector.Run(mca.ctx)
	panic("unreachable")
}

func (mca *MaroonedPodsControllerApp) setupLeaderElector() (err error) {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&v14.EventSinkImpl{Interface: mca.maroonedpodsCli.CoreV1().Events(v1.NamespaceAll)})
	rl, err := resourcelock.New(mca.LeaderElection.ResourceLock,
		mca.maroonedpodsNs,
		"maroonedpods-controller",
		mca.maroonedpodsCli.CoreV1(),
		mca.maroonedpodsCli.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      mca.host,
			EventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, k8sv1.EventSource{Component: "maroonedpods-controller"}),
		})

	if err != nil {
		return
	}

	mca.leaderElector, err = leaderelection.NewLeaderElector(
		leaderelection.LeaderElectionConfig{
			Lock:          rl,
			LeaseDuration: mca.LeaderElection.LeaseDuration.Duration,
			RenewDeadline: mca.LeaderElection.RenewDeadline.Duration,
			RetryPeriod:   mca.LeaderElection.RetryPeriod.Duration,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: mca.onStartedLeading(),
				OnStoppedLeading: func() {
					golog.Fatal("leaderelection lost")
				},
			},
		})

	return
}

func (mca *MaroonedPodsControllerApp) onStartedLeading() func(ctx context.Context) {
	return func(ctx context.Context) {
		stop := ctx.Done()

		go mca.podInformer.Run(stop)
		go mca.maroonedpodsInformer.Run(stop)

		if !cache.WaitForCacheSync(stop,
			mca.podInformer.HasSynced,
			mca.maroonedpodsInformer.HasSynced,
		) {
			klog.Warningf("failed to wait for caches to sync")
		}

		go func() {
			mca.maroonedPodsGateController.Run(context.Background(), 3)
		}()
		close(mca.readyChan)
	}
}

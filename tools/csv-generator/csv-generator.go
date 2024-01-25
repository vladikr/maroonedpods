//Licensed under the Apache License, Version 2.0 (the "License");
//you may not use this file except in compliance with the License.
//You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
//Unless required by applicable law or agreed to in writing, software
//distributed under the License is distributed on an "AS IS" BASIS,
//WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//See the License for the specific language governing permissions and
//limitations under the License.

package main

import (
	"flag"
	"os"

	mpoperator "maroonedpods.io/pkg/maroonedpods-operator/resources/operator"
	"maroonedpods.io/tools/util"
)

var (
	csvVersion         = flag.String("csv-version", "", "")
	replacesCsvVersion = flag.String("replaces-csv-version", "", "")
	namespace          = flag.String("namespace", "", "")
	pullPolicy         = flag.String("pull-policy", "", "")

	maroonedPodsLogoBase64 = flag.String("maroonedpods-logo-base64", "", "")
	verbosity     = flag.String("verbosity", "1", "")

	operatorVersion = flag.String("operator-version", "", "")

	operatorImage   = flag.String("operator-image", "", "")
	controllerImage = flag.String("controller-image", "", "")
	maroonedPodsServerImage  = flag.String("maroonedpods-server-image", "", "")
	dumpCRDs        = flag.Bool("dump-crds", false, "optional - dumps maroonedpods-operator related crd manifests to stdout")
)

func main() {
	flag.Parse()

	data := mpoperator.ClusterServiceVersionData{
		CsvVersion:         *csvVersion,
		ReplacesCsvVersion: *replacesCsvVersion,
		Namespace:          *namespace,
		ImagePullPolicy:    *pullPolicy,
		IconBase64:         *maroonedPodsLogoBase64,
		Verbosity:          *verbosity,

		OperatorVersion: *operatorVersion,

		ControllerImage:    *controllerImage,
		WebhookServerImage: *maroonedPodsServerImage,
		OperatorImage:      *operatorImage,
	}

	csv, err := mpoperator.NewClusterServiceVersion(&data)
	if err != nil {
		panic(err)
	}
	if err = util.MarshallObject(csv, os.Stdout); err != nil {
		panic(err)
	}

	if *dumpCRDs {
		cidCrd := mpoperator.NewMaroonedPodsCrd()
		if err = util.MarshallObject(cidCrd, os.Stdout); err != nil {
			panic(err)
		}
	}
}

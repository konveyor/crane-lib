### How to develop a plugin binary

This document covers how to write a plugin binary using crane-lib. It
 requires:
 1. Go development environment setup
 2. Optionally, an overview of the crane toolkit
 
The binary plugin for crane-lib is meant to be a simple Go program in the
 following format:
 
```
package main

import (
	"fmt"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/konveyor/crane-lib/transform"
	"github.com/konveyor/crane-lib/transform/cli"
)

func main() {
	fields := []transform.OptionalFields{
		{
			FlagName: "MyFlag",
			Help:     "What the flag does",
			Example:  "true",
		},
	}
	cli.RunAndExit(cli.NewCustomPlugin("MyCustomPlugin", "v1", fields, Run))
}

func Run(request transform.PluginRequest) (transform.PluginResponse, error) {
	// plugin writers need to write custome code here.
	resp := transform.PluginResponse{}
	// prepare the response
	return resp, nil
}
```

All it does is read an input from stdin, calls the Run function with the
input object passed as unstructured and prints the return value of `Run`
function on stdout.

The json passed in via stdin is a `transform.PluginRequest` which consists of an
inline unstructured object and an optional `Extras` map containing additional
flags. Without any `Extras` the format is identical to the json output from
a `kubectl get -o json` call. When adding extra params, a map field "extras"
is added at the top level (parallel to "apiVersion", "kind", etc.).

During the development of the plugin, one can iterate by passing in the JSON
object on stdin manually. For example, if the above code is compiled and
 run, this will be the output  
```
./my-plugin
{
   "apiVersion": "route.openshift.io/v1",
   "kind": "Route",
   "metadata": {
      "annotations": {
         "openshift.io/host.generated": "true"
      },
      "creationTimestamp": "2021-06-10T04:11:21Z",
      "managedFields": [
         {
            "apiVersion": "route.openshift.io/v1",
            "fieldsType": "FieldsV1",
            "fieldsV1": {
               "f:spec": {
                  "f:path": {},
                  "f:to": {
                     "f:kind": {},
                     "f:name": {},
                     "f:weight": {}
                  },
                  "f:wildcardPolicy": {}
               }
            },
            "manager": "oc",
            "operation": "Update",
            "time": "2021-06-10T04:11:21Z"
         },
         {
            "apiVersion": "route.openshift.io/v1",
            "fieldsType": "FieldsV1",
            "fieldsV1": {
               "f:status": {
                  "f:ingress": {}
               }
            },
            "manager": "openshift-router",
            "operation": "Update",
            "time": "2021-06-10T04:11:21Z"
         }
      ],
      "name": "mssql-app-route",
      "namespace": "mssql-persistent",
      "resourceVersion": "155816271",
      "selfLink": "/apis/route.openshift.io/v1/namespaces/mssql-persistent/routes/mssql-app-route",
      "uid": "42dca205-31bf-463d-b516-f84064523c2c"
   },
   "spec": {
      "host": "mssql-app-route-mssql-persistent.apps.cluster-alpatel-aux-tools-444.alpatel-aux-tools-444.mg.dog8code.com",
      "path": "/",
      "to": {
         "kind": "Service",
         "name": "mssql-app-service",
         "weight": 100
      },
      "wildcardPolicy": "None"
   },
   "status": {
      "ingress": [
         {
            "conditions": [
               {
                  "lastTransitionTime": "2021-06-10T04:11:21Z",
                  "status": "True",
                  "type": "Admitted"
               }
            ],
            "host": "mssql-app-route-mssql-persistent.apps.cluster-alpatel-aux-tools-444.alpatel-aux-tools-444.mg.dog8code.com",
            "routerCanonicalHostname": "apps.cluster-alpatel-aux-tools-444.alpatel-aux-tools-444.mg.dog8code.com",
            "routerName": "default",
            "wildcardPolicy": "None"
         }
      ]
   }
}
```

Note: the json object was entered from console, and the response was `{}` 

From here, one can iterate over the plugin development. Once the plugin is
ready to be tested, it can be put in a directory and run with the crane cli
command.
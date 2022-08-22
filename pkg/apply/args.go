/*
Copyright 2022 cuisongliu@qq.com.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apply

type Cluster struct {
	Masters     string
	Nodes       string
	ClusterName string
}

type SSH struct {
	User       string
	Password   string
	Pk         string
	PkPassword string
	Port       uint16
}

type RunArgs struct {
	Cluster
	SSH
	CustomEnv []string
	CustomCMD []string
}

type Args struct {
	Values    []string
	Sets      []string
	CustomEnv []string
}

type ResetArgs struct {
	Cluster
	SSH
}

type ScaleArgs struct {
	Cluster
}

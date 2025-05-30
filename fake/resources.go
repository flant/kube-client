package fake

// set current kube-context to cluster with necessary version and run go generate
// it will create file with desired version and resources
// you can use existing cluster or kind/minikube/microk8s/etc
// like: kind create cluster --image "kindest/node:v1.27.3"
// you can images for kind here, in a release message: https://github.com/kubernetes-sigs/kind/releases

//go:generate ./scripts/resources_generator

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterResources returns cluster resources depends on k8s version
func ClusterResources(version ClusterVersion) []*metav1.APIResourceList {
	switch version {
	case ClusterVersionV116:
		return v116ClusterResources

	case ClusterVersionV117:
		return v117ClusterResources

	case ClusterVersionV118:
		return v118ClusterResources

	case ClusterVersionV119:
		return v119ClusterResources

	case ClusterVersionV120:
		return v120ClusterResources

	case ClusterVersionV121:
		return v121ClusterResources

	case ClusterVersionV122:
		return v122ClusterResources

	case ClusterVersionV123:
		return v123ClusterResources

	case ClusterVersionV124:
		return v124ClusterResources

	case ClusterVersionV125:
		return v125ClusterResources

	case ClusterVersionV126:
		return v126ClusterResources

	case ClusterVersionV127:
		return v127ClusterResources

	case ClusterVersionV128:
		return v128ClusterResources
	case ClusterVersionV129:
		return v129ClusterResources
	case ClusterVersionV130:
		return v130ClusterResources
	case ClusterVersionV131:
		return v131ClusterResources
	case ClusterVersionV132:
		return v132ClusterResources
	}

	return nil
}

// ClusterVersion k8s cluster version
type ClusterVersion string

const (
	ClusterVersionV116 ClusterVersion = "v1.16.0"
	ClusterVersionV117 ClusterVersion = "v1.17.0"
	ClusterVersionV118 ClusterVersion = "v1.18.0"
	ClusterVersionV119 ClusterVersion = "v1.19.0"
	ClusterVersionV120 ClusterVersion = "v1.20.0"
	ClusterVersionV121 ClusterVersion = "v1.21.0"
	ClusterVersionV122 ClusterVersion = "v1.22.0"
	ClusterVersionV123 ClusterVersion = "v1.23.0"
	ClusterVersionV124 ClusterVersion = "v1.24.0"
	ClusterVersionV125 ClusterVersion = "v1.25.0"
	ClusterVersionV126 ClusterVersion = "v1.26.0"
	ClusterVersionV127 ClusterVersion = "v1.27.0"
	ClusterVersionV128 ClusterVersion = "v1.28.0"
	ClusterVersionV129 ClusterVersion = "v1.29.0"
	ClusterVersionV130 ClusterVersion = "v1.30.0"
	ClusterVersionV131 ClusterVersion = "v1.31.0"
	ClusterVersionV132 ClusterVersion = "v1.32.0"
)

func (cv ClusterVersion) String() string {
	return string(cv)
}

func (cv ClusterVersion) Major() string {
	return string(cv)[1:2]
}

func (cv ClusterVersion) Minor() string {
	return string(cv)[3:5]
}

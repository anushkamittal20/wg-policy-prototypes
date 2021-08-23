/*
Copyright The Kubernetes Authors.

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

// Code generated by lister-gen. DO NOT EDIT.

package v1alpha1

import (
	v1alpha1 "sigs.k8s.io/wg-policy-prototypes/policy-report/pkg/api/wgpolicyk8s.io/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// ClusterPolicyReportLister helps list ClusterPolicyReports.
// All objects returned here must be treated as read-only.
type ClusterPolicyReportLister interface {
	// List lists all ClusterPolicyReports in the indexer.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1alpha1.ClusterPolicyReport, err error)
	// Get retrieves the ClusterPolicyReport from the index for a given name.
	// Objects returned here must be treated as read-only.
	Get(name string) (*v1alpha1.ClusterPolicyReport, error)
	ClusterPolicyReportListerExpansion
}

// clusterPolicyReportLister implements the ClusterPolicyReportLister interface.
type clusterPolicyReportLister struct {
	indexer cache.Indexer
}

// NewClusterPolicyReportLister returns a new ClusterPolicyReportLister.
func NewClusterPolicyReportLister(indexer cache.Indexer) ClusterPolicyReportLister {
	return &clusterPolicyReportLister{indexer: indexer}
}

// List lists all ClusterPolicyReports in the indexer.
func (s *clusterPolicyReportLister) List(selector labels.Selector) (ret []*v1alpha1.ClusterPolicyReport, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1alpha1.ClusterPolicyReport))
	})
	return ret, err
}

// Get retrieves the ClusterPolicyReport from the index for a given name.
func (s *clusterPolicyReportLister) Get(name string) (*v1alpha1.ClusterPolicyReport, error) {
	obj, exists, err := s.indexer.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(v1alpha1.Resource("clusterpolicyreport"), name)
	}
	return obj.(*v1alpha1.ClusterPolicyReport), nil
}

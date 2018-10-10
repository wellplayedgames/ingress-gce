/*
Copyright 2017 The Kubernetes Authors.

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

package gce

import (
	computealpha "google.golang.org/api/compute/v0.alpha"
	computebeta "google.golang.org/api/compute/v0.beta"
	compute "google.golang.org/api/compute/v1"

	"k8s.io/kubernetes/pkg/cloudprovider/providers/gce/cloud"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/gce/cloud/filter"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/gce/cloud/meta"
)

func newBackendBucketMetricContext(request, region string) *metricContext {
	return newBackendBucketMetricContextWithVersion(request, region, computeV1Version)
}

func newBackendBucketMetricContextWithVersion(request, region, version string) *metricContext {
	return newGenericMetricContext("backendbucket", request, region, unusedMetricLabel, version)
}

// GetBackendBucket retrieves a backend by name.
func (gce *GCECloud) GetBackendBucket(name string) (*compute.BackendBucket, error) {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContext("get", "")
	v, err := gce.c.BackendBuckets().Get(ctx, meta.GlobalKey(name))
	return v, mc.Observe(err)
}

// GetBetaBackendBucket retrieves beta backend by name.
func (gce *GCECloud) GetBetaBackendBucket(name string) (*computebeta.BackendBucket, error) {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContextWithVersion("get", "", computeBetaVersion)
	v, err := gce.c.BetaBackendBuckets().Get(ctx, meta.GlobalKey(name))
	return v, mc.Observe(err)
}

// GetAlphaBackendBucket retrieves alpha backend by name.
func (gce *GCECloud) GetAlphaBackendBucket(name string) (*computealpha.BackendBucket, error) {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContextWithVersion("get", "", computeAlphaVersion)
	v, err := gce.c.AlphaBackendBuckets().Get(ctx, meta.GlobalKey(name))
	return v, mc.Observe(err)
}

// UpdateBackendBucket applies the given BackendBucket as an update to
// an existing bucket.
func (gce *GCECloud) UpdateBackendBucket(bg *compute.BackendBucket) error {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContext("update", "")
	return mc.Observe(gce.c.BackendBuckets().Update(ctx, meta.GlobalKey(bg.Name), bg))
}

// UpdateBetaBackendBucket applies the given beta BackendBucket as an
// update to an existing bucket.
func (gce *GCECloud) UpdateBetaBackendBucket(bg *computebeta.BackendBucket) error {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContextWithVersion("update", "", computeBetaVersion)
	return mc.Observe(gce.c.BetaBackendBuckets().Update(ctx, meta.GlobalKey(bg.Name), bg))
}

// UpdateAlphaBackendBucket applies the given alpha BackendBucket as an
// update to an existing bucket.
func (gce *GCECloud) UpdateAlphaBackendBucket(bg *computealpha.BackendBucket) error {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContextWithVersion("update", "", computeAlphaVersion)
	return mc.Observe(gce.c.AlphaBackendBuckets().Update(ctx, meta.GlobalKey(bg.Name), bg))
}

// DeleteBackendBucket deletes the given BackendBucket by name.
func (gce *GCECloud) DeleteBackendBucket(name string) error {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContext("delete", "")
	return mc.Observe(gce.c.BackendBuckets().Delete(ctx, meta.GlobalKey(name)))
}

// CreateBackendBucket creates the given BackendBucket.
func (gce *GCECloud) CreateBackendBucket(bg *compute.BackendBucket) error {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContext("create", "")
	return mc.Observe(gce.c.BackendBuckets().Insert(ctx, meta.GlobalKey(bg.Name), bg))
}

// CreateBetaBackendBucket creates the given beta BackendBucket.
func (gce *GCECloud) CreateBetaBackendBucket(bg *computebeta.BackendBucket) error {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContextWithVersion("create", "", computeBetaVersion)
	return mc.Observe(gce.c.BetaBackendBuckets().Insert(ctx, meta.GlobalKey(bg.Name), bg))
}

// CreateAlphaBackendBucket creates the given alpha BackendBucket.
func (gce *GCECloud) CreateAlphaBackendBucket(bg *computealpha.BackendBucket) error {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContextWithVersion("create", "", computeAlphaVersion)
	return mc.Observe(gce.c.AlphaBackendBuckets().Insert(ctx, meta.GlobalKey(bg.Name), bg))
}

// ListBackendBuckets lists all backend buckets in the project.
func (gce *GCECloud) ListBackendBuckets() ([]*compute.BackendBucket, error) {
	ctx, cancel := cloud.ContextWithCallTimeout()
	defer cancel()

	mc := newBackendBucketMetricContext("list", "")
	v, err := gce.c.BackendBuckets().List(ctx, filter.None)
	return v, mc.Observe(err)
}

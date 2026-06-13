package gcp

import (
	"context"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/googleapis/gax-go/v2"
)

// gaxCallOption aliases the SDK call-option type so the narrow instancesAPI
// interface does not leak the gax import into every test.
type gaxCallOption = gax.CallOption

// restAPI adapts *compute.InstancesClient to instancesAPI. The concrete client
// returns *compute.Operation / *compute.InstanceIterator, which satisfy the
// operation and instanceIterator interfaces structurally.
type restAPI struct {
	c *compute.InstancesClient
}

func (a restAPI) Insert(ctx context.Context, req *computepb.InsertInstanceRequest, opts ...gaxCallOption) (operation, error) {
	return a.c.Insert(ctx, req, opts...)
}

func (a restAPI) Delete(ctx context.Context, req *computepb.DeleteInstanceRequest, opts ...gaxCallOption) (operation, error) {
	return a.c.Delete(ctx, req, opts...)
}

func (a restAPI) List(ctx context.Context, req *computepb.ListInstancesRequest, opts ...gaxCallOption) instanceIterator {
	return a.c.List(ctx, req, opts...)
}

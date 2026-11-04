package engine

import (
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Image is one of an app's built images, as Docker reports it.
type Image struct {
	ID   string
	Tags []string // e.g. ["ops-platform/hello:<sha>"]; none once a rebuild of the same tag has moved it on
}

// AppImages lists the images built for app: those carrying its LabelApp,
// which Build sets on every image it builds. Filtered on Docker's side, so
// no other image on the host reaches the platform.
func (c *Client) AppImages(ctx context.Context, app string) ([]Image, error) {
	res, err := c.api.ImageList(ctx, client.ImageListOptions{
		Filters: client.Filters{}.Add("label", LabelApp+"="+app),
	})
	if err != nil {
		return nil, fmt.Errorf("listing images: %w", err)
	}
	out := make([]Image, 0, len(res.Items))
	for _, s := range res.Items {
		out = append(out, Image{ID: s.ID, Tags: s.RepoTags})
	}
	return out, nil
}

// ImageExists reports whether ref (a tag or an image ID) is an image Docker
// has.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	_, err := c.api.ImageInspect(ctx, ref)
	switch {
	case err == nil:
		return true, nil
	case cerrdefs.IsNotFound(err):
		return false, nil
	}
	return false, fmt.Errorf("inspecting image: %w", err)
}

// ErrImageInUse means an image can't be removed because a container (even a
// stopped one) uses it. RemoveImage never forces it.
var ErrImageInUse = errors.New("image is in use by a container")

// RemoveImage removes ref: a tag (Docker deletes the image itself once its
// last tag is gone, and the untagged layers under it) or the ID of an untagged
// image. Never forced, so an image a container still uses stays
// (ErrImageInUse). One that's already gone isn't an error.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.api.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true})
	switch {
	case err == nil, cerrdefs.IsNotFound(err):
		return nil
	case cerrdefs.IsConflict(err):
		return ErrImageInUse
	}
	return fmt.Errorf("removing image: %w", err)
}

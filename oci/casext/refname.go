/*
 * umoci: Umoci Modifies Open Containers' Images
 * Copyright (C) 2017 SUSE LLC.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package casext

import (
	"github.com/apex/log"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

// isKnownMediaType returns whether a media type is known by the spec. This
// probably should be moved somewhere else to avoid going out of date.
func isKnownMediaType(mediaType string) bool {
	return mediaType == ispec.MediaTypeDescriptor ||
		mediaType == ispec.MediaTypeImageManifest ||
		mediaType == ispec.MediaTypeImageIndex ||
		mediaType == ispec.MediaTypeImageLayer ||
		mediaType == ispec.MediaTypeImageLayerGzip ||
		mediaType == ispec.MediaTypeImageLayerNonDistributable ||
		mediaType == ispec.MediaTypeImageLayerNonDistributableGzip ||
		mediaType == ispec.MediaTypeImageConfig
}

// ResolveReference will attempt to resolve all possible descriptor paths to
// Manifests (or any unknown blobs) that match a particular reference name (if
// descriptors are stored in non-standard blobs, Resolve will be unable to find
// them but will return the top-most unknown descriptor).
// ResolveReference assumes that "reference name" refers to the value of the
// "org.opencontainers.image.ref.name" descriptor annotation. It is recommended
// that if the returned slice of descriptors is greater than zero that the user
// be consulted to resolve the conflict (due to ambiguity in resolution paths).
//
// TODO: How are we meant to implement other restrictions such as the
//       architecture and feature flags? The API will need to change.
func (e Engine) ResolveReference(ctx context.Context, refname string) ([]ispec.Descriptor, error) {
	index, err := e.GetIndex(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "get top-level index")
	}

	// Set of root links that match the given refname.
	var roots []ispec.Descriptor

	// We only consider the case where AnnotationRefName is defined on the
	// top-level of the index tree. While this isn't codified in the spec (at
	// the time of writing -- 1.0.0-rc5) there are some discussions to add this
	// restriction in 1.0.0-rc6.
	for _, descriptor := range index.Manifests {
		// XXX: What should we do if refname == "".
		if descriptor.Annotations[ispec.AnnotationRefName] == refname {
			roots = append(roots, descriptor)
		}
	}

	// The resolved set of descriptors.
	var resolutions []ispec.Descriptor
	for _, root := range roots {
		// Find all manifests or other blobs that are reachable from the given
		// descriptor.
		if err := e.Walk(ctx, root, func(descriptor ispec.Descriptor) error {
			// It is very important that we do not ignore unknown media types
			// here. We only recurse into mediaTypes that are *known* and are
			// also not ispec.MediaTypeImageManifest.
			if isKnownMediaType(descriptor.MediaType) && descriptor.MediaType != ispec.MediaTypeImageManifest {
				return nil
			}

			// Add the resolution and do not recurse any deeper.
			resolutions = append(resolutions, descriptor)
			return ErrSkipDescriptor
		}); err != nil {
			return nil, errors.Wrapf(err, "walk %s", root.Digest)
		}
	}

	log.WithFields(log.Fields{
		"refs": resolutions,
	}).Debugf("casext.ResolveReference(%s) got these descriptors", refname)
	return resolutions, nil
}

// UpdateReference replaces an existing entry for refname with the given
// descriptor. If there are multiple descriptors that match the refname they
// are all replaced with the given descriptor.
func (e Engine) UpdateReference(ctx context.Context, refname string, descriptor ispec.Descriptor) error {
	// Get index to modify.
	index, err := e.GetIndex(ctx)
	if err != nil {
		return errors.Wrap(err, "get top-level index")
	}

	// TODO: Handle refname = "".
	var newIndex []ispec.Descriptor
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations[ispec.AnnotationRefName] != refname {
			newIndex = append(newIndex, descriptor)
		}
	}
	if len(newIndex)-len(index.Manifests) > 1 {
		// Warn users if the operation is going to remove more than one references.
		log.Warn("multiple references match the given reference name -- all of them have been replaced due to this ambiguity")
	}

	// Append the descriptor.
	if descriptor.Annotations == nil {
		descriptor.Annotations = map[string]string{}
	}
	descriptor.Annotations[ispec.AnnotationRefName] = refname
	newIndex = append(newIndex, descriptor)

	// Commit to image.
	index.Manifests = newIndex
	if err := e.PutIndex(ctx, index); err != nil {
		return errors.Wrap(err, "replace index")
	}
	return nil
}

// AddReferences adds entries for refname with the given descriptors, without
// modifying the existing entries.
func (e Engine) AddReferences(ctx context.Context, refname string, descriptors ...ispec.Descriptor) error {
	if len(descriptors) == 0 {
		// Nothing to do.
		return nil
	}

	// Get index to modify.
	index, err := e.GetIndex(ctx)
	if err != nil {
		return errors.Wrap(err, "get top-level index")
	}

	if len(descriptors) > 1 {
		// Warn users that they're intentionally creating ambiguous images.
		log.Warn("umoci has been requested to add multiple descriptors with the same reference name -- this is intentionally creating ambiguity in the OCI image that some tools may be unable to resolve")
	}

	// Modify the descriptors so that they have the right refname.
	// TODO: Handle refname = "".
	var convertedDescriptors []ispec.Descriptor
	for _, descriptor := range descriptors {
		if descriptor.Annotations == nil {
			descriptor.Annotations = map[string]string{}
		}
		descriptor.Annotations[ispec.AnnotationRefName] = refname
		convertedDescriptors = append(convertedDescriptors, descriptor)
	}

	// Commit to image.
	index.Manifests = append(index.Manifests, convertedDescriptors...)
	if err := e.PutIndex(ctx, index); err != nil {
		return errors.Wrap(err, "replace index")
	}
	return nil
}

// DeleteReference removes all entries in the index that match the given
// refname.
func (e Engine) DeleteReference(ctx context.Context, refname string) error {
	// Get index to modify.
	index, err := e.GetIndex(ctx)
	if err != nil {
		return errors.Wrap(err, "get top-level index")
	}

	// TODO: Handle refname = "".
	var newIndex []ispec.Descriptor
	for _, descriptor := range index.Manifests {
		if descriptor.Annotations[ispec.AnnotationRefName] != refname {
			newIndex = append(newIndex, descriptor)
		}
	}
	if len(newIndex)-len(index.Manifests) > 1 {
		// Warn users if the operation is going to remove more than one references.
		log.Warn("multiple references match the given reference name -- all of them have been deleted due to this ambiguity")
	}

	// Commit to image.
	index.Manifests = newIndex
	if err := e.PutIndex(ctx, index); err != nil {
		return errors.Wrap(err, "replace index")
	}
	return nil
}

// ListReferences returns all of the ref.name entries that are specified in the
// top-level index. Note that the list may contain duplicates, due to the
// nature of references in the image-spec.
func (e Engine) ListReferences(ctx context.Context) ([]string, error) {
	// Get index.
	index, err := e.GetIndex(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "get top-level index")
	}

	var refs []string
	for _, descriptor := range index.Manifests {
		ref, ok := descriptor.Annotations[ispec.AnnotationRefName]
		if ok {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}
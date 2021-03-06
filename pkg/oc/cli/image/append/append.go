package append

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"time"

	units "github.com/docker/go-units"
	"github.com/golang/glog"
	"github.com/spf13/cobra"

	"github.com/docker/distribution"
	distributioncontext "github.com/docker/distribution/context"
	"github.com/docker/distribution/manifest/manifestlist"
	"github.com/docker/distribution/manifest/schema1"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/reference"
	"github.com/docker/distribution/registry/client"
	digest "github.com/opencontainers/go-digest"

	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	kcmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions"

	"github.com/openshift/origin/pkg/image/apis/image/docker10"
	imagereference "github.com/openshift/origin/pkg/image/apis/image/reference"
	"github.com/openshift/origin/pkg/image/dockerlayer"
	"github.com/openshift/origin/pkg/image/dockerlayer/add"
	"github.com/openshift/origin/pkg/image/registryclient"
	"github.com/openshift/origin/pkg/image/registryclient/dockercredentials"
)

var (
	desc = templates.LongDesc(`
		Add layers to Docker images

		Modifies an existing image by adding layers or changing configuration and then pushes that
		image to a remote registry. Any inherited layers are streamed from registry to registry 
		without being stored locally. The default docker credentials are used for authenticating 
		to the registries.

		Layers may be provided as arguments to the command and must each be a gzipped tar archive
		representing a filesystem overlay to the inherited images. The archive may contain a "whiteout"
		file (the prefix '.wh.' and the filename) which will hide files in the lower layers. All
		supported filesystem attributes present in the archive will be used as is.

		Metadata about the image (the configuration passed to the container runtime) may be altered
		by passing a JSON string to the --image or --meta options. The --image flag changes what
		the container runtime sees, while the --meta option allows you to change the attributes of
		the image used by the runtime. Use --dry-run to see the result of your changes. You may
		add the --drop-history flag to remove information from the image about the system that 
		built the base image.

		Images in manifest list format will automatically select an image that matches the current
		operating system and architecture unless you use --filter-by-os to select a different image.
		This flag has no effect on regular images.

		Experimental: This command is under active development and may change without notice.`)

	example = templates.Examples(`
# Remove the entrypoint on the mysql:latest image
%[1]s --from mysql:latest --to myregistry.com/myimage:latest --image {"Entrypoint":null}

# Add a new layer to the image
%[1]s --from mysql:latest --to myregistry.com/myimage:latest layer.tar.gz
`)
)

type AppendImageOptions struct {
	From, To   string
	LayerFiles []string

	ConfigPatch string
	MetaPatch   string

	DropHistory bool
	CreatedAt   string

	OSFilter        *regexp.Regexp
	DefaultOSFilter bool

	FilterByOS string

	MaxPerRegistry int

	DryRun   bool
	Insecure bool
	Force    bool

	genericclioptions.IOStreams
}

// schema2ManifestOnly specifically requests a manifest list first
var schema2ManifestOnly = distribution.WithManifestMediaTypes([]string{
	manifestlist.MediaTypeManifestList,
	schema2.MediaTypeManifest,
})

func NewAppendImageOptions(streams genericclioptions.IOStreams) *AppendImageOptions {
	return &AppendImageOptions{
		IOStreams:      streams,
		MaxPerRegistry: 3,
	}
}

// New creates a new command
func NewCmdAppendImage(name string, streams genericclioptions.IOStreams) *cobra.Command {
	o := NewAppendImageOptions(streams)

	cmd := &cobra.Command{
		Use:     "append",
		Short:   "Add layers to images and push them to a registry",
		Long:    desc,
		Example: fmt.Sprintf(example, name),
		Run: func(c *cobra.Command, args []string) {
			kcmdutil.CheckErr(o.Complete(c, args))
			kcmdutil.CheckErr(o.Run())
		},
	}

	flag := cmd.Flags()
	flag.BoolVar(&o.DryRun, "dry-run", o.DryRun, "Print the actions that would be taken and exit without writing to the destination.")
	flag.BoolVar(&o.Insecure, "insecure", o.Insecure, "Allow push and pull operations to registries to be made over HTTP")
	flag.StringVar(&o.FilterByOS, "filter-by-os", o.FilterByOS, "A regular expression to control which images are mirrored. Images will be passed as '<platform>/<architecture>[/<variant>]'.")

	flag.StringVar(&o.From, "from", o.From, "The image to use as a base. If empty, a new scratch image is created.")
	flag.StringVar(&o.To, "to", o.To, "The Docker repository tag to upload the appended image to.")

	flag.StringVar(&o.ConfigPatch, "image", o.ConfigPatch, "A JSON patch that will be used with the output image data.")
	flag.StringVar(&o.MetaPatch, "meta", o.MetaPatch, "A JSON patch that will be used with image base metadata (advanced config).")
	flag.BoolVar(&o.DropHistory, "drop-history", o.DropHistory, "Fields on the image that relate to the history of how the image was created will be removed.")
	flag.StringVar(&o.CreatedAt, "created-at", o.CreatedAt, "The creation date for this image, in RFC3339 format or milliseconds from the Unix epoch.")

	flag.BoolVar(&o.Force, "force", o.Force, "If set, the command will attempt to upload all layers instead of skipping those that are already uploaded.")
	flag.IntVar(&o.MaxPerRegistry, "max-per-registry", o.MaxPerRegistry, "Number of concurrent requests allowed per registry.")

	return cmd
}

func (o *AppendImageOptions) Complete(cmd *cobra.Command, args []string) error {
	pattern := o.FilterByOS
	if len(pattern) == 0 && !cmd.Flags().Changed("filter-by-os") {
		o.DefaultOSFilter = true
		pattern = regexp.QuoteMeta(fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH))
	}
	if len(pattern) > 0 {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("--filter-by-os was not a valid regular expression: %v", err)
		}
		o.OSFilter = re
	}

	for _, arg := range args {
		fi, err := os.Stat(arg)
		if err != nil {
			return fmt.Errorf("invalid argument: %s", err)
		}
		if fi.IsDir() {
			return fmt.Errorf("invalid argument: %s is a directory", arg)
		}
	}
	o.LayerFiles = args

	return nil
}

// includeDescriptor returns true if the provided manifest should be included.
func (o *AppendImageOptions) includeDescriptor(d *manifestlist.ManifestDescriptor, hasMultiple bool) bool {
	if o.OSFilter == nil {
		return true
	}
	if o.DefaultOSFilter && !hasMultiple {
		return true
	}
	if len(d.Platform.Variant) > 0 {
		return o.OSFilter.MatchString(fmt.Sprintf("%s/%s/%s", d.Platform.OS, d.Platform.Architecture, d.Platform.Variant))
	}
	return o.OSFilter.MatchString(fmt.Sprintf("%s/%s", d.Platform.OS, d.Platform.Architecture))
}

func (o *AppendImageOptions) Run() error {
	var createdAt *time.Time
	if len(o.CreatedAt) > 0 {
		if d, err := strconv.ParseInt(o.CreatedAt, 10, 64); err == nil {
			t := time.Unix(d/1000, (d%1000)*1000000).UTC()
			createdAt = &t
		} else {
			t, err := time.Parse(time.RFC3339, o.CreatedAt)
			if err != nil {
				return fmt.Errorf("--created-at must be a relative time (2m, -5h) or an RFC3339 formatted date")
			}
			createdAt = &t
		}
	}

	var from *imagereference.DockerImageReference
	if len(o.From) > 0 {
		src, err := imagereference.Parse(o.From)
		if err != nil {
			return err
		}
		if len(src.Tag) == 0 && len(src.ID) == 0 {
			return fmt.Errorf("--from must point to an image ID or image tag")
		}
		from = &src
	}
	to, err := imagereference.Parse(o.To)
	if err != nil {
		return err
	}
	if len(to.ID) > 0 {
		return fmt.Errorf("--to may not point to an image by ID")
	}

	rt, err := rest.TransportFor(&rest.Config{})
	if err != nil {
		return err
	}
	insecureRT, err := rest.TransportFor(&rest.Config{TLSClientConfig: rest.TLSClientConfig{Insecure: true}})
	if err != nil {
		return err
	}
	creds := dockercredentials.NewLocal()
	ctx := context.Background()
	fromContext := registryclient.NewContext(rt, insecureRT).WithCredentials(creds)
	toContext := registryclient.NewContext(rt, insecureRT).WithActions("push").WithCredentials(creds)

	toRepo, err := toContext.Repository(ctx, to.DockerClientDefaults().RegistryURL(), to.RepositoryName(), o.Insecure)
	if err != nil {
		return err
	}
	toManifests, err := toRepo.Manifests(ctx)
	if err != nil {
		return err
	}

	var (
		base     *docker10.DockerImageConfig
		layers   []distribution.Descriptor
		fromRepo distribution.Repository
	)
	if from != nil {
		repo, err := fromContext.Repository(ctx, from.DockerClientDefaults().RegistryURL(), from.RepositoryName(), o.Insecure)
		if err != nil {
			return err
		}
		fromRepo = repo
		var srcDigest digest.Digest
		if len(from.Tag) > 0 {
			desc, err := repo.Tags(ctx).Get(ctx, from.Tag)
			if err != nil {
				return err
			}
			srcDigest = desc.Digest
		} else {
			srcDigest = digest.Digest(from.ID)
		}
		manifests, err := repo.Manifests(ctx)
		if err != nil {
			return err
		}
		srcManifest, err := manifests.Get(ctx, srcDigest, schema2ManifestOnly)
		if err != nil {
			return err
		}

		originalSrcDigest := srcDigest
		srcManifests, srcManifest, srcDigest, err := processManifestList(ctx, srcDigest, srcManifest, manifests, *from, o.includeDescriptor)
		if err != nil {
			return err
		}
		if len(srcManifests) == 0 {
			return fmt.Errorf("filtered all images from %s", from)
		}

		var location string
		if srcDigest == originalSrcDigest {
			location = fmt.Sprintf("manifest %s", srcDigest)
		} else {
			location = fmt.Sprintf("manifest %s in manifest list %s", srcDigest, originalSrcDigest)
		}

		switch t := srcManifest.(type) {
		case *schema2.DeserializedManifest:
			if t.Config.MediaType != schema2.MediaTypeImageConfig {
				return fmt.Errorf("unable to append layers to images with config %s from %s", t.Config.MediaType, location)
			}
			configJSON, err := repo.Blobs(ctx).Get(ctx, t.Config.Digest)
			if err != nil {
				return fmt.Errorf("unable to find manifest for image %s: %v", *from, err)
			}
			glog.V(4).Infof("Raw image config json:\n%s", string(configJSON))
			config := &docker10.DockerImageConfig{}
			if err := json.Unmarshal(configJSON, &config); err != nil {
				return fmt.Errorf("the source image manifest could not be parsed: %v", err)
			}

			base = config
			layers = t.Layers
			base.Size = 0
			for _, layer := range t.Layers {
				base.Size += layer.Size
			}

		case *schema1.SignedManifest:
			if glog.V(4) {
				_, configJSON, _ := srcManifest.Payload()
				glog.Infof("Raw image config json:\n%s", string(configJSON))
			}
			if len(t.History) == 0 {
				return fmt.Errorf("input image is in an unknown format: no v1Compatibility history")
			}
			config := &docker10.DockerV1CompatibilityImage{}
			if err := json.Unmarshal([]byte(t.History[0].V1Compatibility), &config); err != nil {
				return err
			}

			base = &docker10.DockerImageConfig{}
			if err := docker10.Convert_DockerV1CompatibilityImage_to_DockerImageConfig(config, base); err != nil {
				return err
			}

			// schema1 layers are in reverse order
			layers = make([]distribution.Descriptor, 0, len(t.FSLayers))
			for i := len(t.FSLayers) - 1; i >= 0; i-- {
				layer := distribution.Descriptor{
					MediaType: schema2.MediaTypeLayer,
					Digest:    t.FSLayers[i].BlobSum,
					// size must be reconstructed from the blobs
				}
				// we must reconstruct the tar sum from the blobs
				add.AddLayerToConfig(base, layer, "")
				layers = append(layers, layer)
			}

		default:
			return fmt.Errorf("unable to append layers to images of type %T from %s", srcManifest, location)
		}
	} else {
		base = add.NewEmptyConfig()
		layers = []distribution.Descriptor{add.AddScratchLayerToConfig(base)}
		fromRepo = scratchRepo{}
	}

	if base.Config == nil {
		base.Config = &docker10.DockerConfig{}
	}

	if glog.V(4) {
		configJSON, _ := json.MarshalIndent(base, "", "  ")
		glog.Infof("input config:\n%s\nlayers: %#v", configJSON, layers)
	}

	if createdAt == nil {
		t := time.Now()
		createdAt = &t
	}
	base.Created = *createdAt
	if o.DropHistory {
		base.ContainerConfig = docker10.DockerConfig{}
		base.History = nil
		base.Container = ""
		base.DockerVersion = ""
		base.Config.Image = ""
	}

	if len(o.ConfigPatch) > 0 {
		if err := json.Unmarshal([]byte(o.ConfigPatch), base.Config); err != nil {
			return fmt.Errorf("unable to patch image from --image: %v", err)
		}
	}
	if len(o.MetaPatch) > 0 {
		if err := json.Unmarshal([]byte(o.MetaPatch), base); err != nil {
			return fmt.Errorf("unable to patch image from --meta: %v", err)
		}
	}

	numLayers := len(layers)
	toBlobs := toRepo.Blobs(ctx)

	for _, arg := range o.LayerFiles {
		err := func() error {
			f, err := os.Open(arg)
			if err != nil {
				return err
			}
			defer f.Close()
			var readerFrom io.ReaderFrom = ioutil.Discard.(io.ReaderFrom)
			var done = func(distribution.Descriptor) error { return nil }
			if !o.DryRun {
				fmt.Fprint(o.Out, "Uploading ... ")
				start := time.Now()
				bw, err := toBlobs.Create(ctx)
				if err != nil {
					fmt.Fprintln(o.Out, "failed")
					return err
				}
				readerFrom = bw
				defer bw.Close()
				done = func(desc distribution.Descriptor) error {
					_, err := bw.Commit(ctx, desc)
					if err != nil {
						fmt.Fprintln(o.Out, "failed")
						return err
					}
					fmt.Fprintf(o.Out, "%s/s\n", units.HumanSize(float64(desc.Size)/float64(time.Now().Sub(start))*float64(time.Second)))
					return nil
				}
			}
			layerDigest, blobDigest, modTime, n, err := add.DigestCopy(readerFrom, f)
			desc := distribution.Descriptor{
				Digest:    blobDigest,
				Size:      n,
				MediaType: schema2.MediaTypeLayer,
			}
			layers = append(layers, desc)
			add.AddLayerToConfig(base, desc, layerDigest.String())
			if modTime != nil && !modTime.IsZero() {
				base.Created = *modTime
			}
			return done(desc)
		}()
		if err != nil {
			return err
		}
	}

	if o.DryRun {
		configJSON, _ := json.MarshalIndent(base, "", "  ")
		fmt.Fprintf(o.Out, "%s", configJSON)
		return nil
	}

	// upload base layers in parallel
	stopCh := make(chan struct{})
	defer close(stopCh)
	q := newWorkQueue(o.MaxPerRegistry, stopCh)
	err = q.Try(func(w Try) {
		for i := range layers[:numLayers] {
			layer := &layers[i]
			index := i
			missingDiffID := len(base.RootFS.DiffIDs[i]) == 0
			w.Try(func() error {
				fromBlobs := fromRepo.Blobs(ctx)

				// check whether the blob exists
				if !o.Force {
					if desc, err := toBlobs.Stat(ctx, layer.Digest); err == nil {
						// ensure the correct size makes it back to the manifest
						glog.V(4).Infof("Layer %s already exists in destination (%s)", layer.Digest, units.HumanSizeWithPrecision(float64(layer.Size), 3))
						if layer.Size == 0 {
							layer.Size = desc.Size
						}
						// we need to calculate the tar sum from the image, requiring us to pull it
						if missingDiffID {
							glog.V(4).Infof("Need tar sum, streaming layer %s", layer.Digest)
							r, err := fromBlobs.Open(ctx, layer.Digest)
							if err != nil {
								return fmt.Errorf("unable to access the layer %s in order to calculate its content ID: %v", layer.Digest, err)
							}
							defer r.Close()
							layerDigest, _, _, _, err := add.DigestCopy(ioutil.Discard.(io.ReaderFrom), r)
							if err != nil {
								return fmt.Errorf("unable to calculate contentID for layer %s: %v", layer.Digest, err)
							}
							glog.V(4).Infof("Layer %s has tar sum %s", layer.Digest, layerDigest)
							base.RootFS.DiffIDs[index] = layerDigest.String()
						}
						// TODO: due to a bug in the registry, the empty layer is always returned as existing, but
						// an upload without it will fail - https://bugzilla.redhat.com/show_bug.cgi?id=1599028
						if layer.Digest != dockerlayer.GzippedEmptyLayerDigest {
							return nil
						}
					}
				}

				// source
				r, err := fromBlobs.Open(ctx, layer.Digest)
				if err != nil {
					return fmt.Errorf("unable to access the source layer %s: %v", layer.Digest, err)
				}
				defer r.Close()

				// destination
				mountOptions := []distribution.BlobCreateOption{WithDescriptor(*layer)}
				if from != nil && from.Registry == to.Registry {
					source, err := reference.WithDigest(fromRepo.Named(), layer.Digest)
					if err != nil {
						return err
					}
					mountOptions = append(mountOptions, client.WithMountFrom(source))
				}
				bw, err := toBlobs.Create(ctx, mountOptions...)
				if err != nil {
					return fmt.Errorf("unable to upload layer %s to destination repository: %v", layer.Digest, err)
				}
				defer bw.Close()

				// copy the blob, calculating the diffID if necessary
				if layer.Size > 0 {
					fmt.Fprintf(o.Out, "Uploading %s ...\n", units.HumanSize(float64(layer.Size)))
				} else {
					fmt.Fprintf(o.Out, "Uploading ...\n")
				}
				if missingDiffID {
					glog.V(4).Infof("Need tar sum, calculating while streaming %s", layer.Digest)
					layerDigest, _, _, _, err := add.DigestCopy(bw, r)
					if err != nil {
						return err
					}
					glog.V(4).Infof("Layer %s has tar sum %s", layer.Digest, layerDigest)
					base.RootFS.DiffIDs[index] = layerDigest.String()
				} else {
					if _, err := bw.ReadFrom(r); err != nil {
						return fmt.Errorf("unable to copy the source layer %s to the destination image: %v", layer.Digest, err)
					}
				}
				desc, err := bw.Commit(ctx, *layer)
				if err != nil {
					return fmt.Errorf("uploading the source layer %s failed: %v", layer.Digest, err)
				}

				// check output
				if desc.Digest != layer.Digest {
					return fmt.Errorf("when uploading blob %s, got a different returned digest %s", desc.Digest, layer.Digest)
				}
				// ensure the correct size makes it back to the manifest
				if layer.Size == 0 {
					layer.Size = desc.Size
				}
				return nil
			})
		}
	})
	if err != nil {
		return err
	}

	manifest, err := add.UploadSchema2Config(ctx, toBlobs, base, layers)
	if err != nil {
		return fmt.Errorf("unable to upload the new image manifest: %v", err)
	}
	toDigest, err := putManifestInCompatibleSchema(ctx, manifest, to.Tag, toManifests, fromRepo.Blobs(ctx), toRepo.Named())
	if err != nil {
		return fmt.Errorf("unable to convert the image to a compatible schema version: %v", err)
	}
	fmt.Fprintf(o.Out, "Pushed image %s to %s\n", toDigest, to)
	return nil
}

type optionFunc func(interface{}) error

func (f optionFunc) Apply(v interface{}) error {
	return f(v)
}

// WithDescriptor returns a BlobCreateOption which provides the expected blob metadata.
func WithDescriptor(desc distribution.Descriptor) distribution.BlobCreateOption {
	return optionFunc(func(v interface{}) error {
		opts, ok := v.(*distribution.CreateOptions)
		if !ok {
			return fmt.Errorf("unexpected options type: %T", v)
		}
		if opts.Mount.Stat == nil {
			opts.Mount.Stat = &desc
		}
		return nil
	})
}

func calculateLayerDigest(blobs distribution.BlobService, dgst digest.Digest, readerFrom io.ReaderFrom, r io.Reader) (digest.Digest, error) {
	if readerFrom == nil {
		readerFrom = ioutil.Discard.(io.ReaderFrom)
	}
	layerDigest, _, _, _, err := add.DigestCopy(readerFrom, r)
	return layerDigest, err
}

// scratchRepo can serve the scratch image blob.
type scratchRepo struct{}

var _ distribution.Repository = scratchRepo{}

func (_ scratchRepo) Named() reference.Named { panic("not implemented") }
func (_ scratchRepo) Tags(ctx distributioncontext.Context) distribution.TagService {
	panic("not implemented")
}
func (_ scratchRepo) Manifests(ctx distributioncontext.Context, options ...distribution.ManifestServiceOption) (distribution.ManifestService, error) {
	panic("not implemented")
}

func (r scratchRepo) Blobs(ctx distributioncontext.Context) distribution.BlobStore { return r }

func (_ scratchRepo) Stat(ctx distributioncontext.Context, dgst digest.Digest) (distribution.Descriptor, error) {
	if dgst != dockerlayer.GzippedEmptyLayerDigest {
		return distribution.Descriptor{}, distribution.ErrBlobUnknown
	}
	return distribution.Descriptor{
		MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip",
		Digest:    digest.Digest(dockerlayer.GzippedEmptyLayerDigest),
		Size:      int64(len(dockerlayer.GzippedEmptyLayer)),
	}, nil
}

func (_ scratchRepo) Get(ctx distributioncontext.Context, dgst digest.Digest) ([]byte, error) {
	if dgst != dockerlayer.GzippedEmptyLayerDigest {
		return nil, distribution.ErrBlobUnknown
	}
	return dockerlayer.GzippedEmptyLayer, nil
}

type nopCloseBuffer struct {
	*bytes.Buffer
}

func (_ nopCloseBuffer) Seek(offset int64, whence int) (int64, error) {
	return 0, nil
}

func (_ nopCloseBuffer) Close() error {
	return nil
}

func (_ scratchRepo) Open(ctx distributioncontext.Context, dgst digest.Digest) (distribution.ReadSeekCloser, error) {
	if dgst != dockerlayer.GzippedEmptyLayerDigest {
		return nil, distribution.ErrBlobUnknown
	}
	return nopCloseBuffer{bytes.NewBuffer(dockerlayer.GzippedEmptyLayer)}, nil
}

func (_ scratchRepo) Put(ctx distributioncontext.Context, mediaType string, p []byte) (distribution.Descriptor, error) {
	panic("not implemented")
}

func (_ scratchRepo) Create(ctx distributioncontext.Context, options ...distribution.BlobCreateOption) (distribution.BlobWriter, error) {
	panic("not implemented")
}

func (_ scratchRepo) Resume(ctx distributioncontext.Context, id string) (distribution.BlobWriter, error) {
	panic("not implemented")
}

func (_ scratchRepo) ServeBlob(ctx distributioncontext.Context, w http.ResponseWriter, r *http.Request, dgst digest.Digest) error {
	panic("not implemented")
}

func (_ scratchRepo) Delete(ctx distributioncontext.Context, dgst digest.Digest) error {
	panic("not implemented")
}

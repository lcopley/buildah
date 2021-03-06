package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/containers/buildah/imagebuildah"
	buildahcli "github.com/containers/buildah/pkg/cli"
	"github.com/containers/buildah/pkg/formats"
	is "github.com/containers/image/storage"
	"github.com/containers/storage"
	units "github.com/docker/go-units"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type jsonImage struct {
	ID    string   `json:"id"`
	Names []string `json:"names"`
}

type imageOutputParams struct {
	Tag          string
	ID           string
	Name         string
	Digest       string
	CreatedAt    string
	Size         string
	CreatedAtRaw time.Time
}

type imageOptions struct {
	all       bool
	digests   bool
	format    string
	json      bool
	noHeading bool
	truncate  bool
	quiet     bool
}

type filterParams struct {
	dangling         string
	label            string
	beforeImage      string
	sinceImage       string
	beforeDate       time.Time
	sinceDate        time.Time
	referencePattern string
}

type imageResults struct {
	imageOptions
	filter string
}

var imagesHeader = map[string]string{
	"Name":      "REPOSITORY",
	"Tag":       "TAG",
	"ID":        "IMAGE ID",
	"CreatedAt": "CREATED",
	"Size":      "SIZE",
}

func init() {
	var (
		opts              imageResults
		imagesDescription = "\n  Lists locally stored images."
	)
	imagesCommand := &cobra.Command{
		Use:   "images",
		Short: "List images in local storage",
		Long:  imagesDescription,
		RunE: func(cmd *cobra.Command, args []string) error {
			return imagesCmd(cmd, args, &opts)
		},
		Example: `buildah images --all
  buildah images [imageName]
  buildah images --format '{{.ID}} {{.Name}} {{.Size}} {{.CreatedAtRaw}}'`,
	}
	imagesCommand.SetUsageTemplate(UsageTemplate())

	flags := imagesCommand.Flags()
	flags.SetInterspersed(false)
	flags.BoolVarP(&opts.all, "all", "a", false, "show all images, including intermediate images from a build")
	flags.BoolVar(&opts.digests, "digests", false, "show digests")
	flags.StringVarP(&opts.filter, "filter", "f", "", "filter output based on conditions provided")
	flags.StringVar(&opts.format, "format", "", "pretty-print images using a Go template")
	flags.BoolVar(&opts.json, "json", false, "output in JSON format")
	flags.BoolVarP(&opts.noHeading, "noheading", "n", false, "do not print column headings")
	// TODO needs alias here -- to `notruncate`
	flags.BoolVar(&opts.truncate, "no-trunc", false, "do not truncate output")
	flags.BoolVarP(&opts.quiet, "quiet", "q", false, "display only image IDs")

	rootCmd.AddCommand(imagesCommand)
}

func imagesCmd(c *cobra.Command, args []string, iopts *imageResults) error {

	name := ""
	if len(args) > 0 {
		if iopts.all {
			return errors.Errorf("when using the --all switch, you may not pass any images names or IDs")
		}

		if err := buildahcli.VerifyFlagsArgsOrder(args); err != nil {
			return err
		}
		if len(args) == 1 {
			name = args[0]
		} else {
			return errors.New("'buildah images' requires at most 1 argument")
		}
	}

	store, err := getStore(c)
	if err != nil {
		return err
	}

	images, err := store.Images()
	if err != nil {
		return errors.Wrapf(err, "error reading images")
	}

	if iopts.quiet && iopts.format != "" {
		return errors.Errorf("quiet and format are mutually exclusive")
	}

	opts := imageOptions{
		all:       iopts.all,
		digests:   iopts.digests,
		format:    iopts.format,
		json:      iopts.json,
		noHeading: iopts.noHeading,
		truncate:  !iopts.truncate,
		quiet:     iopts.quiet,
	}
	ctx := getContext()

	var params *filterParams
	if iopts.filter != "" {
		params, err = parseFilter(ctx, store, images, iopts.filter)
		if err != nil {
			return errors.Wrapf(err, "error parsing filter")
		}
	}

	return outputImages(ctx, images, store, params, name, opts)
}

func parseFilter(ctx context.Context, store storage.Store, images []storage.Image, filter string) (*filterParams, error) {
	params := new(filterParams)
	filterStrings := strings.Split(filter, ",")
	for _, param := range filterStrings {
		pair := strings.SplitN(param, "=", 2)
		switch strings.TrimSpace(pair[0]) {
		case "dangling":
			if pair[1] == "true" || pair[1] == "false" {
				params.dangling = pair[1]
			} else {
				return nil, fmt.Errorf("invalid filter: '%s=[%s]'", pair[0], pair[1])
			}
		case "label":
			params.label = pair[1]
		case "before":
			beforeDate, err := setFilterDate(ctx, store, images, pair[1])
			if err != nil {
				return nil, fmt.Errorf("no such id: %s", pair[0])
			}
			params.beforeDate = beforeDate
			params.beforeImage = pair[1]
		case "since":
			sinceDate, err := setFilterDate(ctx, store, images, pair[1])
			if err != nil {
				return nil, fmt.Errorf("no such id: %s", pair[0])
			}
			params.sinceDate = sinceDate
			params.sinceImage = pair[1]
		case "reference":
			params.referencePattern = pair[1]
		default:
			return nil, fmt.Errorf("invalid filter: '%s'", pair[0])
		}
	}
	return params, nil
}

func setFilterDate(ctx context.Context, store storage.Store, images []storage.Image, imgName string) (time.Time, error) {
	for _, image := range images {
		for _, name := range image.Names {
			if matchesReference(name, imgName) {
				// Set the date to this image
				ref, err := is.Transport.ParseStoreReference(store, image.ID)
				if err != nil {
					return time.Time{}, fmt.Errorf("error parsing reference to image %q: %v", image.ID, err)
				}
				img, err := ref.NewImage(ctx, nil)
				if err != nil {
					return time.Time{}, fmt.Errorf("error reading image %q: %v", image.ID, err)
				}
				defer img.Close()
				inspect, err := img.Inspect(ctx)
				if err != nil {
					return time.Time{}, fmt.Errorf("error inspecting image %q: %v", image.ID, err)
				}
				date := *inspect.Created
				return date, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("Could not locate image %q", imgName)
}

func outputHeader(opts imageOptions) string {
	if opts.format != "" {
		return strings.Replace(opts.format, `\t`, "\t", -1)
	}
	if opts.quiet {
		return formats.IDString
	}
	format := "table {{.Name}}\t{{.Tag}}\t"
	if opts.noHeading {
		format = "{{.Name}}\t{{.Tag}}\t"
	}

	if opts.digests {
		format += "{{.Digest}}\t"
	}
	format += "{{.ID}}\t{{.CreatedAt}}\t{{.Size}}"
	return format
}

type imagesSorted []imageOutputParams

func outputImages(ctx context.Context, images []storage.Image, store storage.Store, filters *filterParams, argName string, opts imageOptions) error {
	found := false
	var imagesParams imagesSorted
	jsonImages := []jsonImage{}
	for _, image := range images {
		createdTime := image.Created
		inspectedTime, digest, size, _ := getDateAndDigestAndSize(ctx, image, store)
		if !inspectedTime.IsZero() {
			if createdTime != inspectedTime {
				logrus.Debugf("image record and configuration disagree on the image's creation time for %q, using the one from the configuration", image)
				createdTime = inspectedTime
			}
		}
		createdTime = createdTime.Local()

		// If all is false and the image doesn't have a name, check to see if the top layer of the image is a parent
		// to another image's top layer. If it is, then it is an intermediate image so don't print out if the --all flag
		// is not set.
		if !opts.all && len(image.Names) == 0 {
			isParent, err := imageIsParent(store, image.TopLayer)
			if err != nil {
				logrus.Errorf("error checking if image is a parent %q: %v", image.ID, err)
			}
			if isParent {
				continue
			}
		}

		imageID := "sha256:" + image.ID
		if opts.truncate {
			imageID = shortID(image.ID)
		}

		names := []string{}
		if len(image.Names) > 0 {
			names = image.Names
		} else {
			// images without names should be printed with "<none>" as the image name
			names = append(names, "<none>:<none>")
		}

	outer:
		for name, tags := range imagebuildah.ReposToMap(names) {
			for _, tag := range tags {
				if !matchesReference(name+":"+tag, argName) {
					continue
				}
				found = true

				if !matchesFilter(ctx, image, store, name+":"+tag, filters) {
					continue
				}
				if opts.json {
					jsonImages = append(jsonImages, jsonImage{ID: image.ID, Names: image.Names})
					// We only want to print each id once
					break outer
				}
				params := imageOutputParams{
					Tag:          tag,
					ID:           imageID,
					Name:         name,
					Digest:       digest,
					CreatedAtRaw: createdTime,
					CreatedAt:    units.HumanDuration(time.Since((createdTime))) + " ago",
					Size:         formattedSize(size),
				}
				imagesParams = append(imagesParams, params)
				if opts.quiet {
					// We only want to print each id once
					break outer
				}
			}
		}
	}

	if !found && argName != "" {
		return errors.Errorf("No such image %s", argName)
	}
	if opts.json {
		data, err := json.MarshalIndent(jsonImages, "", "    ")
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", data)
		return nil
	}
	imagesParams = sortImagesOutput(imagesParams)
	out := formats.StdoutTemplateArray{Output: imagesToGeneric(imagesParams), Template: outputHeader(opts), Fields: imagesHeader}
	formats.Writer(out).Out()
	return nil
}

func shortID(id string) string {
	idTruncLength := 12
	if len(id) > idTruncLength {
		return id[:idTruncLength]
	}
	return id
}

func sortImagesOutput(imagesOutput imagesSorted) imagesSorted {
	sort.Sort(imagesOutput)
	return imagesOutput
}

func (a imagesSorted) Less(i, j int) bool {
	return a[i].CreatedAtRaw.After(a[j].CreatedAtRaw)
}
func (a imagesSorted) Len() int      { return len(a) }
func (a imagesSorted) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

func imagesToGeneric(templParams []imageOutputParams) (genericParams []interface{}) {
	if len(templParams) > 0 {
		for _, v := range templParams {
			genericParams = append(genericParams, interface{}(v))
		}
	}
	return genericParams
}

func matchesFilter(ctx context.Context, image storage.Image, store storage.Store, name string, params *filterParams) bool {
	if params == nil {
		return true
	}
	if params.dangling != "" && !matchesDangling(name, params.dangling) {
		return false
	} else if params.label != "" && !matchesLabel(ctx, image, store, params.label) {
		return false
	} else if params.beforeImage != "" && !matchesBeforeImage(image, name, params) {
		return false
	} else if params.sinceImage != "" && !matchesSinceImage(image, name, params) {
		return false
	} else if params.referencePattern != "" && !matchesReference(name, params.referencePattern) {
		return false
	}
	return true
}

func matchesDangling(name string, dangling string) bool {
	if dangling == "false" && !strings.Contains(name, "<none>") {
		return true
	} else if dangling == "true" && strings.Contains(name, "<none>") {
		return true
	}
	return false
}

func matchesLabel(ctx context.Context, image storage.Image, store storage.Store, label string) bool {
	storeRef, err := is.Transport.ParseStoreReference(store, image.ID)
	if err != nil {
		return false
	}
	img, err := storeRef.NewImage(ctx, nil)
	if err != nil {
		return false
	}
	defer img.Close()
	info, err := img.Inspect(ctx)
	if err != nil {
		return false
	}

	pair := strings.SplitN(label, "=", 2)
	for key, value := range info.Labels {
		if key == pair[0] {
			if len(pair) == 2 {
				if value == pair[1] {
					return true
				}
			} else {
				return false
			}
		}
	}
	return false
}

// Returns true if the image was created since the filter image.  Returns
// false otherwise
func matchesBeforeImage(image storage.Image, name string, params *filterParams) bool {
	return image.Created.IsZero() || image.Created.Before(params.beforeDate)
}

// Returns true if the image was created since the filter image.  Returns
// false otherwise
func matchesSinceImage(image storage.Image, name string, params *filterParams) bool {
	return image.Created.IsZero() || image.Created.After(params.sinceDate)
}

func matchesID(imageID, argID string) bool {
	return strings.HasPrefix(imageID, argID)
}

func matchesReference(name, argName string) bool {
	if argName == "" {
		return true
	}
	splitName := strings.Split(name, ":")
	// If the arg contains a tag, we handle it differently than if it does not
	if strings.Contains(argName, ":") {
		splitArg := strings.Split(argName, ":")
		return strings.HasSuffix(splitName[0], splitArg[0]) && (splitName[1] == splitArg[1])
	}
	return strings.HasSuffix(splitName[0], argName)
}

/*
According to  https://en.wikipedia.org/wiki/Binary_prefix
We should be return numbers based on 1000, rather then 1024
*/
func formattedSize(size int64) string {
	suffixes := [5]string{"B", "KB", "MB", "GB", "TB"}

	count := 0
	formattedSize := float64(size)
	for formattedSize >= 1000 && count < 4 {
		formattedSize /= 1000
		count++
	}
	return fmt.Sprintf("%.3g %s", formattedSize, suffixes[count])
}

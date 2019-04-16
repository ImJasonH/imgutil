package imgutil

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/pkg/errors"
)

type RemoteImage struct {
	keychain   authn.Keychain
	RepoName   string
	Image      v1.Image
	PrevLayers []v1.Layer
	prevOnce   *sync.Once
}

func NewRemoteImage(repoName string, keychain authn.Keychain) (*RemoteImage, error) {
	image, err := newV1Image(keychain, repoName)
	if err != nil {
		return nil, err
	}

	return &RemoteImage{
		keychain: keychain,
		RepoName: repoName,
		Image:    image,
		prevOnce: &sync.Once{},
	}, nil
}

func newV1Image(keychain authn.Keychain, repoName string) (v1.Image, error) {
	ref, auth, err := referenceForRepoName(keychain, repoName)
	if err != nil {
		return nil, err
	}
	image, err := remote.Image(ref, remote.WithAuth(auth))
	if err != nil {
		return nil, fmt.Errorf("connect to repo store '%s': %s", repoName, err.Error())
	}
	return image, nil
}

func referenceForRepoName(keychain authn.Keychain, ref string) (name.Reference, authn.Authenticator, error) {
	var auth authn.Authenticator
	r, err := name.ParseReference(ref, name.WeakValidation)
	if err != nil {
		return nil, nil, err
	}

	auth, err = keychain.Resolve(r.Context().Registry)
	if err != nil {
		return nil, nil, err
	}
	return r, auth, nil
}

func (r *RemoteImage) Label(key string) (string, error) {
	cfg, err := r.Image.ConfigFile()
	if err != nil || cfg == nil {
		return "", fmt.Errorf("failed to get label, image '%s' does not exist", r.RepoName)
	}
	labels := cfg.Config.Labels
	return labels[key], nil

}

func (r *RemoteImage) Env(key string) (string, error) {
	cfg, err := r.Image.ConfigFile()
	if err != nil || cfg == nil {
		return "", fmt.Errorf("failed to get env var, image '%s' does not exist", r.RepoName)
	}
	for _, envVar := range cfg.Config.Env {
		parts := strings.Split(envVar, "=")
		if parts[0] == key {
			return parts[1], nil
		}
	}
	return "", nil
}

func (r *RemoteImage) Rename(name string) {
	r.RepoName = name
}

func (r *RemoteImage) Name() string {
	return r.RepoName
}

func (r *RemoteImage) Found() (bool, error) {
	if _, err := r.Image.RawManifest(); err != nil {
		if transportErr, ok := err.(*transport.Error); ok && len(transportErr.Errors) > 0 {
			switch transportErr.Errors[0].Code {
			case transport.UnauthorizedErrorCode, transport.ManifestUnknownErrorCode:
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
}

func (r *RemoteImage) Digest() (string, error) {
	hash, err := r.Image.Digest()
	if err != nil {
		return "", fmt.Errorf("failed to get digest for image '%s': %s", r.RepoName, err)
	}
	return hash.String(), nil
}

func (r *RemoteImage) CreatedAt() (time.Time, error) {
	configFile, err := r.Image.ConfigFile()
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to get createdAt time for image '%s': %s", r.RepoName, err)
	}
	return configFile.Created.UTC(), nil
}

func (r *RemoteImage) Rebase(baseTopLayer string, newBase Image) error {
	newBaseRemote, ok := newBase.(*RemoteImage)
	if !ok {
		return errors.New("expected new base to be a remote image")
	}

	newImage, err := mutate.Rebase(r.Image, &subImage{img: r.Image, topSHA: baseTopLayer}, newBaseRemote.Image)
	if err != nil {
		return errors.Wrap(err, "rebase")
	}
	r.Image = newImage
	return nil
}

func (r *RemoteImage) SetLabel(key, val string) error {
	configFile, err := r.Image.ConfigFile()
	if err != nil {
		return err
	}
	config := *configFile.Config.DeepCopy()
	if config.Labels == nil {
		config.Labels = map[string]string{}
	}
	config.Labels[key] = val
	r.Image, err = mutate.Config(r.Image, config)
	return err
}

func (r *RemoteImage) SetEnv(key, val string) error {
	configFile, err := r.Image.ConfigFile()
	if err != nil {
		return err
	}
	config := *configFile.Config.DeepCopy()
	for i, e := range config.Env {
		parts := strings.Split(e, "=")
		if parts[0] == key {
			config.Env[i] = fmt.Sprintf("%s=%s", key, val)
			r.Image, err = mutate.Config(r.Image, config)
			if err != nil {
				return err
			}
			return nil
		}
	}
	config.Env = append(config.Env, fmt.Sprintf("%s=%s", key, val))
	r.Image, err = mutate.Config(r.Image, config)
	return err
}

func (r *RemoteImage) SetEntrypoint(ep ...string) error {
	configFile, err := r.Image.ConfigFile()
	if err != nil {
		return err
	}
	config := *configFile.Config.DeepCopy()
	config.Entrypoint = ep
	r.Image, err = mutate.Config(r.Image, config)
	return err
}

func (r *RemoteImage) SetCmd(cmd ...string) error {
	configFile, err := r.Image.ConfigFile()
	if err != nil {
		return err
	}
	config := *configFile.Config.DeepCopy()
	config.Cmd = cmd
	r.Image, err = mutate.Config(r.Image, config)
	return err
}

func (r *RemoteImage) TopLayer() (string, error) {
	all, err := r.Image.Layers()
	if err != nil {
		return "", err
	}
	topLayer := all[len(all)-1]
	hex, err := topLayer.DiffID()
	if err != nil {
		return "", err
	}
	return hex.String(), nil
}

func (r *RemoteImage) GetLayer(string) (io.ReadCloser, error) {
	panic("not implemented")
}

func (r *RemoteImage) AddLayer(path string) error {
	layer, err := tarball.LayerFromFile(path)
	if err != nil {
		return err
	}
	r.Image, err = mutate.AppendLayers(r.Image, layer)
	if err != nil {
		return errors.Wrap(err, "add layer")
	}
	return nil
}

func (r *RemoteImage) ReuseLayer(sha string) error {
	var outerErr error

	r.prevOnce.Do(func() {
		prevImage, err := newV1Image(r.keychain, r.RepoName)
		if err != nil {
			outerErr = err
			return
		}
		r.PrevLayers, err = prevImage.Layers()
		if err != nil {
			outerErr = fmt.Errorf("failed to get layers for previous image with repo name '%s': %s", r.RepoName, err)
		}
	})
	if outerErr != nil {
		return outerErr
	}

	layer, err := findLayerWithSha(r.PrevLayers, sha)
	if err != nil {
		return err
	}
	r.Image, err = mutate.AppendLayers(r.Image, layer)
	return err
}

func findLayerWithSha(layers []v1.Layer, sha string) (v1.Layer, error) {
	for _, layer := range layers {
		diffID, err := layer.DiffID()
		if err != nil {
			return nil, errors.Wrap(err, "get diff ID for previous image layer")
		}
		if sha == diffID.String() {
			return layer, nil
		}
	}
	return nil, fmt.Errorf(`previous image did not have layer with sha '%s'`, sha)
}

func (r *RemoteImage) Save() (string, error) {
	ref, auth, err := referenceForRepoName(r.keychain, r.RepoName)
	if err != nil {
		return "", err
	}

	r.Image, err = mutate.CreatedAt(r.Image, v1.Time{Time: time.Now()})
	if err != nil {
		return "", err
	}

	if err := remote.Write(ref, r.Image, auth, http.DefaultTransport); err != nil {
		return "", err
	}

	hex, err := r.Image.Digest()
	if err != nil {
		return "", err
	}

	return hex.String(), nil
}

func (r *RemoteImage) Delete() error {
	return errors.New("remote image does not implement Delete")
}

type subImage struct {
	img    v1.Image
	topSHA string
}

func (si *subImage) Layers() ([]v1.Layer, error) {
	all, err := si.img.Layers()
	if err != nil {
		return nil, err
	}
	for i, l := range all {
		d, err := l.DiffID()
		if err != nil {
			return nil, err
		}
		if d.String() == si.topSHA {
			return all[:i+1], nil
		}
	}
	return nil, errors.New("could not find base layer in image")
}
func (si *subImage) BlobSet() (map[v1.Hash]struct{}, error)  { panic("Not Implemented") }
func (si *subImage) MediaType() (types.MediaType, error)     { panic("Not Implemented") }
func (si *subImage) ConfigName() (v1.Hash, error)            { panic("Not Implemented") }
func (si *subImage) ConfigFile() (*v1.ConfigFile, error)     { panic("Not Implemented") }
func (si *subImage) RawConfigFile() ([]byte, error)          { panic("Not Implemented") }
func (si *subImage) Digest() (v1.Hash, error)                { panic("Not Implemented") }
func (si *subImage) Manifest() (*v1.Manifest, error)         { panic("Not Implemented") }
func (si *subImage) RawManifest() ([]byte, error)            { panic("Not Implemented") }
func (si *subImage) LayerByDigest(v1.Hash) (v1.Layer, error) { panic("Not Implemented") }
func (si *subImage) LayerByDiffID(v1.Hash) (v1.Layer, error) { panic("Not Implemented") }

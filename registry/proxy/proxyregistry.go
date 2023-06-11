package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/configuration"
	dcontext "github.com/distribution/distribution/v3/context"
	"github.com/distribution/distribution/v3/reference"
	"github.com/distribution/distribution/v3/registry/client"
	"github.com/distribution/distribution/v3/registry/client/auth"
	"github.com/distribution/distribution/v3/registry/client/auth/challenge"
	"github.com/distribution/distribution/v3/registry/client/transport"
	"github.com/distribution/distribution/v3/registry/proxy/scheduler"
	"github.com/distribution/distribution/v3/registry/storage"
	"github.com/distribution/distribution/v3/registry/storage/driver"
)

// proxyingRegistry fetches content from a remote registry and caches it locally
type proxyingRegistry struct {
	embedded         distribution.Namespace // provides local registry functionality
	scheduler        *scheduler.TTLExpirationScheduler
	remoteURL        url.URL
	enableNamespaces bool
	authChallenger   authChallenger
}

// NewRegistryPullThroughCache creates a registry acting as a pull through cache
func NewRegistryPullThroughCache(ctx context.Context, registry distribution.Namespace, driver driver.StorageDriver, config configuration.Proxy) (distribution.Namespace, error) {
	remoteURL, err := url.Parse(config.RemoteURL) // this value is null and acts as a placeholder
	if err != nil {
		return nil, err
	}

	v := storage.NewVacuum(ctx, driver)
	s := scheduler.New(ctx, driver, "/scheduler-state.json")
	s.OnBlobExpire(func(ref reference.Reference) error {
		var r reference.Canonical
		var ok bool
		if r, ok = ref.(reference.Canonical); !ok {
			return fmt.Errorf("unexpected reference type : %T", ref)
		}

		repo, err := registry.Repository(ctx, r)
		if err != nil {
			return err
		}

		blobs := repo.Blobs(ctx)

		// Clear the repository reference and descriptor caches
		err = blobs.Delete(ctx, r.Digest())
		if err != nil {
			return err
		}

		err = v.RemoveBlob(r.Digest().String())
		if err != nil {
			return err
		}

		return nil
	})

	s.OnManifestExpire(func(ref reference.Reference) error {
		var r reference.Canonical
		var ok bool
		if r, ok = ref.(reference.Canonical); !ok {
			return fmt.Errorf("unexpected reference type : %T", ref)
		}

		repo, err := registry.Repository(ctx, r)
		if err != nil {
			return err
		}

		manifests, err := repo.Manifests(ctx)
		if err != nil {
			return err
		}
		err = manifests.Delete(ctx, r.Digest())
		if err != nil {
			return err
		}
		return nil
	})

	err = s.Start()
	if err != nil {
		return nil, err
	}

	if !config.EnableNamespaces {
		config.NamespaceCredentials = map[string]configuration.ProxyCredential{
			config.RemoteURL: {
				Username: config.Username,
				Password: config.Password,
			},
		}
	}

	cs, err := configureAuth(config.NamespaceCredentials)
	if err != nil {
		return nil, err
	}

	return &proxyingRegistry{
		embedded:         registry,
		scheduler:        s,
		remoteURL:        *remoteURL,
		enableNamespaces: config.EnableNamespaces,
		authChallenger: &remoteAuthChallenger{
			remoteURL:        *remoteURL,
			enableNamespaces: config.EnableNamespaces,
			cm:               challenge.NewSimpleManager(),
			cs:               cs,
		},
	}, nil
}

func (pr *proxyingRegistry) Scope() distribution.Scope {
	return distribution.GlobalScope
}

func (pr *proxyingRegistry) Repositories(ctx context.Context, repos []string, last string) (n int, err error) {
	return pr.embedded.Repositories(ctx, repos, last)
}

func (pr *proxyingRegistry) Repository(ctx context.Context, name reference.Named) (distribution.Repository, error) {
	c := pr.authChallenger

	tkopts := auth.TokenHandlerOptions{
		Transport:   http.DefaultTransport,
		Credentials: c.credentialStore(),
		Scopes: []auth.Scope{
			auth.RepositoryScope{
				Repository: name.Name(),
				Actions:    []string{"pull"},
			},
		},
		Logger: dcontext.GetLogger(ctx),
	}

	tr := transport.NewTransport(http.DefaultTransport,
		auth.NewAuthorizer(c.challengeManager(),
			auth.NewTokenHandlerWithOptions(tkopts)))

	localName := name         // registry-1.docker.io/library/redis
	remoteURL := pr.remoteURL // null
	if pr.enableNamespaces {
		var err error
		remoteURL, name, err = extractRemoteURL(ctx) // https   registry-1.docker.io   %!s(bool=false) %!s(bool=false)
		if err != nil {
			return nil, err
		}

		localName, err = reference.WithName(remoteURL.Host + "/" + name.Name()) //
		if err != nil {
			return nil, err
		}
	}

	// name: bitnami/aws-cli
	// localName: registry-1.docker.io/library/redis
	// remoteURL https://public.ecr.aws

	localRepo, err := pr.embedded.Repository(ctx, localName)
	if err != nil {
		return nil, err
	}
	localManifests, err := localRepo.Manifests(ctx, storage.SkipLayerVerification())
	if err != nil {
		return nil, err
	}

	remoteRepo, err := client.NewRepository(name, remoteURL.String(), tr)
	if err != nil {
		return nil, err
	}

	remoteManifests, err := remoteRepo.Manifests(ctx)
	if err != nil {
		return nil, err
	}

	dcontext.GetLogger(ctx).Infof("New localManifests: %s", localManifests)
	dcontext.GetLogger(ctx).Infof("New remoteManifests: %s", remoteManifests)

	return &proxiedRepository{
		blobStore: &proxyBlobStore{
			localStore:     localRepo.Blobs(ctx),
			remoteStore:    remoteRepo.Blobs(ctx),
			scheduler:      pr.scheduler,
			repositoryName: localName,
			authChallenger: pr.authChallenger,
		},
		manifests: &proxyManifestStore{
			repositoryName:  localName,
			localManifests:  localManifests, // Options?
			remoteManifests: remoteManifests,
			ctx:             ctx,
			scheduler:       pr.scheduler,
			authChallenger:  pr.authChallenger,
		},
		name: name,
		tags: &proxyTagService{
			localTags:      localRepo.Tags(ctx),
			remoteTags:     remoteRepo.Tags(ctx),
			authChallenger: pr.authChallenger,
		},
	}, nil
}

func (pr *proxyingRegistry) Blobs() distribution.BlobEnumerator {
	return pr.embedded.Blobs()
}

func (pr *proxyingRegistry) BlobStatter() distribution.BlobStatter {
	return pr.embedded.BlobStatter()
}

// authChallenger encapsulates a request to the upstream to establish credential challenges
type authChallenger interface {
	tryEstablishChallenges(context.Context) error
	challengeManager() challenge.Manager
	credentialStore() auth.CredentialStore
}

type remoteAuthChallenger struct {
	remoteURL        url.URL
	enableNamespaces bool
	sync.Mutex
	cm challenge.Manager
	cs auth.CredentialStore
}

func (r *remoteAuthChallenger) credentialStore() auth.CredentialStore {
	return r.cs
}

func (r *remoteAuthChallenger) challengeManager() challenge.Manager {
	return r.cm
}

// tryEstablishChallenges will attempt to get a challenge type for the upstream if none currently exist
func (r *remoteAuthChallenger) tryEstablishChallenges(ctx context.Context) error {
	r.Lock()
	defer r.Unlock()

	remoteURL := r.remoteURL
	if r.enableNamespaces {
		requestRemoteNSURL, _, err := extractRemoteURL(ctx)
		if err != nil {
			return err
		}
		remoteURL = requestRemoteNSURL
	}

	// if remoteURL.Host != "gcr.io" {
	remoteURL.Path = "/v2/"
	// }

	challenges, err := r.cm.GetChallenges(remoteURL)
	if err != nil {
		return err
	}

	if len(challenges) > 0 {
		return nil
	}

	// establish challenge type with upstream
	if err := ping(r.cm, remoteURL.String(), challengeHeader); err != nil {
		return err
	}

	dcontext.GetLogger(ctx).Infof("Challenge established with upstream : %s | %s", remoteURL.String(), r.cm)
	return nil
}

// proxiedRepository uses proxying blob and manifest services to serve content
// locally, or pulling it through from a remote and caching it locally if it doesn't
// already exist
type proxiedRepository struct {
	blobStore distribution.BlobStore
	manifests distribution.ManifestService
	name      reference.Named
	tags      distribution.TagService
}

func (pr *proxiedRepository) Manifests(ctx context.Context, options ...distribution.ManifestServiceOption) (distribution.ManifestService, error) {
	return pr.manifests, nil
}

func (pr *proxiedRepository) Blobs(ctx context.Context) distribution.BlobStore {
	return pr.blobStore
}

func (pr *proxiedRepository) Named() reference.Named {
	return pr.name
}

func (pr *proxiedRepository) Tags(ctx context.Context) distribution.TagService {
	return pr.tags
}

func extractRemoteURL(ctx context.Context) (url.URL, reference.Named, error) {
	r, err := dcontext.GetRequest(ctx)
	if err != nil {
		return url.URL{}, nil, err
	}

	ns := r.URL.Query().Get("ns")
	name := dcontext.GetStringValue(ctx, "vars.name") // public.ecr.aws/bitnami/aws-cli
	if ns == "" {
		// When the ns parameter is missing, assume that the domain is already prepended to the image name
		var found bool
		ns, name, found = strings.Cut(name, "/")
		if !found || strings.IndexRune(ns, '.') < 1 {
			return url.URL{}, nil, errors.New("ns parameter is missing and image is not prefixed with domain")
		}
	}

	// if ns == "docker.io" {
	// 	ns = "registry-1.docker.io"
	// }

	named, err := reference.WithName(name)
	if err != nil {
		return url.URL{}, nil, err
	}

	return url.URL{
		Scheme: "https",
		Host:   ns,
	}, named, nil
}

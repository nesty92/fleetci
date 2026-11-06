package gitlab

// gitlab-runner registers its cache adapters, secret resolvers and shells
// from its main package; these are main.go's imports of v19.5.2, without
// them a runner's [runners.cache] and a job's secrets do nothing.
import (
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/azure"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/gcs"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/gcsv2"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/s3"
	_ "gitlab.com/gitlab-org/gitlab-runner/cache/s3v2"
	_ "gitlab.com/gitlab-org/gitlab-runner/helpers/secrets/resolvers/aws"
	_ "gitlab.com/gitlab-org/gitlab-runner/helpers/secrets/resolvers/azure_key_vault"
	_ "gitlab.com/gitlab-org/gitlab-runner/helpers/secrets/resolvers/gcp_secret_manager"
	_ "gitlab.com/gitlab-org/gitlab-runner/helpers/secrets/resolvers/gitlab_secrets_manager"
	_ "gitlab.com/gitlab-org/gitlab-runner/helpers/secrets/resolvers/vault"
	_ "gitlab.com/gitlab-org/gitlab-runner/shells"
)

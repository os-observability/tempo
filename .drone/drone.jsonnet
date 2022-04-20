local apps = ['tempo', 'tempo-vulture', 'tempo-query'];
local archs = ['amd64', 'arm64'];

//# Building blocks ##

local pipeline(name, arch='amd64') = {
  kind: 'pipeline',
  name: name,
  platform: {
    os: 'linux',
    arch: arch,
  },
  steps: [],
  depends_on: [],
  trigger: {
    ref: [
      'refs/heads/main',
      'refs/tags/v*',
      // weekly release branches
      'refs/heads/r?',
      'refs/heads/r??',
    ],
  },
};

local secret(name, vault_path, vault_key) = {
  kind: 'secret',
  name: name,
  get: {
    path: vault_path,
    name: vault_key,
  },
};

local docker_username_secret = secret('docker_username', 'infra/data/ci/docker_hub', 'username');
local docker_password_secret = secret('docker_password', 'infra/data/ci/docker_hub', 'password');

// secrets for pushing serverless code packages
local fn_upload_ops_tools_secret = secret('ops_tools_fn_upload', 'infra/data/ci/tempo-ops-tools-function-upload', 'credentials.json');

// secret needed to access us.gcr.io in deploy_to_dev()
local docker_config_json_secret = secret('dockerconfigjson', 'secret/data/common/gcr', '.dockerconfigjson');

// secret needed for dep-tools
local gh_token_secret = secret('gh_token', 'infra/data/ci/github/grafanabot', 'pat');

// gcs buckets to copy serverless functions to
local gcp_secrets = [fn_upload_ops_tools_secret.name];
local gcp_serverless_deployments = [
  {
    bucket: 'ops-tools-tempo-function-source',
    secret: fn_upload_ops_tools_secret.name,
  },
  {
    bucket: 'grafanalabs-global-tempo-function-source',
    secret: fn_upload_ops_tools_secret.name,
  },
];

local aws_dev_access_key_id = secret('AWS_ACCESS_KEY_ID-dev', 'infra/data/ci/tempo-dev/aws-credentials-drone', 'access_key_id');
local aws_dev_secret_access_key = secret('AWS_SECRET_ACCESS_KEY-dev', 'infra/data/ci/tempo-dev/aws-credentials-drone', 'secret_access_key');
local aws_prod_access_key_id = secret('AWS_ACCESS_KEY_ID-prod', 'infra/data/ci/tempo-prod/aws-credentials-drone', 'access_key_id');
local aws_prod_secret_access_key = secret('AWS_SECRET_ACCESS_KEY-prod', 'infra/data/ci/tempo-prod/aws-credentials-drone', 'secret_access_key');


local aws_serverless_deployments = [
  {
    env: 'dev',
    bucket: 'dev-tempo-fn-source',
    access_key_id: aws_dev_access_key_id.name,
    secret_access_key: aws_dev_secret_access_key.name,
  },
  {
    env: 'prod',
    bucket: 'prod-tempo-fn-source',
    access_key_id: aws_prod_access_key_id.name,
    secret_access_key: aws_prod_secret_access_key.name,
  },
];


//# Steps ##

// the alpine/git image has apk errors when run on aarch64, this is the most recent image that does not have this issue
// https://github.com/alpine-docker/git/issues/35
local alpine_git_image = 'alpine/git:v2.30.2';

local image_tag(arch='') = {
  name: 'image-tag',
  image: alpine_git_image,
  commands: [
    'apk --update --no-cache add bash',
    'git fetch origin --tags',
  ] + (
    if arch == '' then [
      'echo $(./tools/image-tag) > .tags',
    ] else [
      'echo $(./tools/image-tag)-%s > .tags' % arch,
    ]
  ),
};

local image_tag_for_cd() = {
  name: 'image-tag-for-cd',
  image: alpine_git_image,
  commands: [
    'apk --update --no-cache add bash',
    'git fetch origin --tags',
    'echo "grafana/tempo:$(./tools/image-tag)" > .tags-for-cd-tempo',
    'echo "grafana/tempo-query:$(./tools/image-tag)" > .tags-for-cd-tempo_query',
    'echo "grafana/tempo-vulture:$(./tools/image-tag)" > .tags-for-cd-tempo_vulture',
  ],
};

local build_binaries(arch) = {
  name: 'build-tempo-binaries',
  image: 'golang:1.17-alpine',
  commands: [
    'apk add make git',
  ] + [
    'COMPONENT=%s GOARCH=%s make exe' % [app, arch]
    for app in apps
  ],
};

local docker_build(arch, app) = {
  name: 'build-%s-image' % app,
  image: 'plugins/docker',
  settings: {
    dockerfile: 'cmd/%s/Dockerfile' % app,
    repo: 'grafana/%s' % app,
    username: { from_secret: docker_username_secret.name },
    password: { from_secret: docker_password_secret.name },
    build_args: [
      'TARGETARCH=' + arch,
    ],
  },
};

local docker_manifest(app) = {
  name: 'manifest-%s' % app,
  image: 'plugins/manifest',
  settings: {
    username: { from_secret: docker_username_secret.name },
    password: { from_secret: docker_password_secret.name },
    spec: '.drone/docker-manifest.tmpl',
    target: app,
  },
};

local deploy_to_dev() = {
  image: 'us.gcr.io/kubernetes-dev/drone/plugins/updater',
  name: 'update-dev-images',
  settings: {
    config_json: std.manifestJsonEx(
      {
        destination_branch: 'master',
        pull_request_branch_prefix: 'cd-tempo-dev',
        pull_request_enabled: false,
        pull_request_team_reviewers: [
          'tempo',
        ],
        repo_name: 'deployment_tools',
        update_jsonnet_attribute_configs: [
          {
            file_path: 'ksonnet/environments/tempo/dev-us-central-0.tempo-dev-01/images.libsonnet',
            jsonnet_key: app,
            jsonnet_value_file: '.tags-for-cd-' + app,
          }
          for app in ['tempo', 'tempo_query', 'tempo_vulture']
        ],
      },
      '  '
    ),
    github_token: {
      from_secret: gh_token_secret.name,
    },
  },
};

//# Pipelines & resources

[
  // A pipeline to build Docker images for every app and for every arch
  (
    pipeline('docker-' + arch, arch) {
      steps+: [
        image_tag(arch),
        build_binaries(arch),
      ] + [
        docker_build(arch, app)
        for app in apps
      ],
    }
  )
  for arch in archs
] + [
  // Publish Docker manifests
  pipeline('manifest') {
    steps+: [
      image_tag(),
    ] + [
      docker_manifest(app)
      for app in apps
    ],
    depends_on+: [
      'docker-%s' % arch
      for arch in archs
    ],
  },
] + [
  // Continuously Deploy to dev env
  pipeline('cd-to-dev-env') {
    trigger: {
      ref: [
        // always deploy tip of main to dev
        'refs/heads/main',
      ],
    },
    image_pull_secrets: [
      docker_config_json_secret.name,
    ],
    steps+: [
      image_tag_for_cd(),
    ] + [
      deploy_to_dev(),
    ],
    depends_on+: [
      // wait for images to be published on dockerhub
      'manifest',
    ],
  },
] + [
  // Build and deploy serverless code packages
  pipeline('build-deploy-serverless') {
    steps+: [
              {
                name: 'build-tempo-serverless',
                image: 'golang:1.17-alpine',
                commands: [
                  'apk add make git zip bash',
                  'cd ./cmd/tempo-serverless',
                  'make build-gcf-zip',
                  'make build-lambda-zip',
                ],
              },
              {
                name: 'deploy-tempo-serverless-gcs',
                image: 'google/cloud-sdk',
                environment: {
                  [s]: {
                    from_secret: s,
                  }
                  for s in gcp_secrets
                },
                commands: [
                  'cd ./cmd/tempo-serverless/cloud-functions',
                ] + [
                  'printf "%%s" "$%s" > ./creds.json && gcloud auth activate-service-account --key-file ./creds.json && gsutil cp tempo-serverless*.zip gs://%s' % [d.secret, d.bucket]
                  for d in gcp_serverless_deployments
                ],
              },
            ] +
            [
              {
                name: 'deploy-tempo-%s-serverless-lambda' % d.env,
                image: 'amazon/aws-cli',
                environment: {
                  AWS_DEFAULT_REGION: 'us-east-2',
                  AWS_ACCESS_KEY_ID: {
                    from_secret: d.access_key_id,
                  },
                  AWS_SECRET_ACCESS_KEY: {
                    from_secret: d.secret_access_key,
                  },
                },
                commands: [
                  'cd ./cmd/tempo-serverless/lambda',
                  'aws s3 cp tempo-serverless*.zip s3://%s' % d.bucket,
                ],
              }

              for d in aws_serverless_deployments
            ],
  },
] + [
  docker_username_secret,
  docker_password_secret,
  docker_config_json_secret,
  gh_token_secret,
  fn_upload_ops_tools_secret,
  aws_dev_access_key_id,
  aws_dev_secret_access_key,
]

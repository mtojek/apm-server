#!/usr/bin/env groovy
@Library('apm@current') _

pipeline {
  agent none
  environment {
    REPO = 'apm-server'
    BASE_DIR = "src/github.com/elastic/${env.REPO}"
    SLACK_CHANNEL = '#apm-server'
    NOTIFY_TO = 'build-apm+apm-server@elastic.co'
    JOB_GCS_BUCKET = credentials('gcs-bucket')
    JOB_GCS_CREDENTIALS = 'apm-ci-gcs-plugin'
    DOCKER_SECRET = 'secret/apm-team/ci/docker-registry/prod'
    DOCKER_REGISTRY = 'docker.elastic.co'
    DRA_OUTPUT = 'release-manager.out'
    COMMIT = "${params?.COMMIT}"
    JOB_GIT_CREDENTIALS = "f6c7695a-671e-4f4f-a331-acdce44ff9ba"
    DOCKER_IMAGE = "${env.DOCKER_REGISTRY}/observability-ci/apm-server"

    // Signing
    JOB_SIGNING_CREDENTIALS = 'sign-artifacts-with-gpg-job'
    INFRA_SIGNING_BUCKET_NAME = 'internal-ci-artifacts'
    INFRA_SIGNING_BUCKET_SIGNED_ARTIFACTS_SUBFOLDER = "${env.REPO_BUILD_TAG}/signed-artifacts"
    INFRA_SIGNING_BUCKET_ARTIFACTS_PATH = "gs://${env.INFRA_SIGNING_BUCKET_NAME}/${env.REPO_BUILD_TAG}"
    INFRA_SIGNING_BUCKET_SIGNED_ARTIFACTS_PATH = "gs://${env.INFRA_SIGNING_BUCKET_NAME}/${env.INFRA_SIGNING_BUCKET_SIGNED_ARTIFACTS_SUBFOLDER}"

    // Publishing
    INTERNAL_CI_JOB_GCS_CREDENTIALS = 'internal-ci-gcs-plugin'
    PACKAGE_STORAGE_UPLOADER_CREDENTIALS = 'upload-package-to-package-storage'
    PACKAGE_STORAGE_UPLOADER_GCP_SERVICE_ACCOUNT = 'secret/gce/elastic-bekitzur/service-account/package-storage-uploader'
    PACKAGE_STORAGE_INTERNAL_BUCKET_QUEUE_PUBLISHING_PATH = "gs://elastic-bekitzur-package-storage-internal/queue-publishing/${env.REPO_BUILD_TAG}"
  }
  options {
    timeout(time: 2, unit: 'HOURS')
    buildDiscarder(logRotator(numToKeepStr: '100', artifactNumToKeepStr: '30', daysToKeepStr: '30'))
    timestamps()
    ansiColor('xterm')
    disableResume()
    durabilityHint('PERFORMANCE_OPTIMIZED')
    rateLimitBuilds(throttle: [count: 60, durationName: 'hour', userBoost: true])
    quietPeriod(10)
  }
  parameters {
    string(name: 'COMMIT', defaultValue: '', description: 'The Git commit to be used (empty will checkout the latest commit)')
  }
  stages {
    stage('Filter build') {
      agent { label 'ubuntu-18 && immutable' }
      options { skipDefaultCheckout() }
      when {
        beforeAgent true
        anyOf {
          triggeredBy cause: "IssueCommentCause"
          expression {
            def ret = isUserTrigger() || isUpstreamTrigger()
            if(!ret){
              currentBuild.result = 'NOT_BUILT'
              currentBuild.description = "The build has been skipped"
              currentBuild.displayName = "#${BUILD_NUMBER}-(Skipped)"
              echo("the build has been skipped due the trigger is a branch scan and the allowed ones are manual, GitHub comment, and upstream job")
            }
            return ret
          }
        }
      }
      environment {
        PATH = "${env.PATH}:${env.WORKSPACE}/bin"
        HOME = "${env.WORKSPACE}"
      }
      stages {
        stage('Checkout') {
          options { skipDefaultCheckout() }
          steps {
            pipelineManager([ cancelPreviousRunningBuilds: [ when: 'PR' ] ])
            deleteDir()
            smartGitCheckout()
            stash(allowEmpty: true, name: 'source', useDefaultExcludes: false)
            // set environment variables globally since they are used afterwards but GIT_BASE_COMMIT won't
            // be available until gitCheckout is executed.
            setEnvVar('URI_SUFFIX', "commits/${env.GIT_BASE_COMMIT}")
            // JOB_GCS_BUCKET contains the bucket and some folders, let's build the folder structure
            setEnvVar('PATH_PREFIX', "${JOB_GCS_BUCKET.contains('/') ? JOB_GCS_BUCKET.substring(JOB_GCS_BUCKET.indexOf('/') + 1) + '/' + env.URI_SUFFIX : env.URI_SUFFIX}")
            setEnvVar('IS_BRANCH_AVAILABLE', isBranchUnifiedReleaseAvailable(env.BRANCH_NAME))
            dir("${BASE_DIR}"){
              setEnvVar('VERSION', sh(label: 'Get version', script: 'make get-version', returnStdout: true)?.trim())
            }
          }
        }
        stage('apmpackage') {
          options { skipDefaultCheckout() }
          steps {
            withGithubNotify(context: 'apmpackage') {
              runWithGo() {
                sh(script: 'make build-package', label: 'legacy: make build-package')
                sh(label: 'legacy: package-storage-snapshot', script: 'make -C .ci/scripts package-storage-snapshot')

                sh(script: 'make build-package-snapshot', label: 'v2: make build-package-snapshot')
                archiveArtifacts(allowEmptyArchive: false, artifacts: 'build/packages/*.zip')
                packageStoragePublish('build/packages')
              }
            }
          }
        }
      }
    }
  }
  post {
    cleanup {
      notifyBuildResult(prComment: false)
    }
  }
}

def runReleaseManager(def args = [:]) {
  deleteDir()
  unstash 'source'
  def bucketLocation = getBucketLocation(args.type)
  googleStorageDownload(bucketUri: "${bucketLocation}/*",
                        credentialsId: "${JOB_GCS_CREDENTIALS}",
                        localDirectory: "${BASE_DIR}/build/distributions",
                        pathPrefix: "${env.PATH_PREFIX}/${args.type}")
  dir("${BASE_DIR}") {
    dockerLogin(secret: env.DOCKER_SECRET, registry: env.DOCKER_REGISTRY)
    sh(label: "prepare-release-manager-artifacts ${args.type}", script: ".ci/scripts/prepare-release-manager.sh ${args.type}")
    releaseManager(project: 'apm-server',
                   version: env.VERSION,
                   type: args.type,
                   artifactsFolder: 'build/distributions',
                   outputFile: args.outputFile)
  }
}

def runWithGo(Closure body) {
  deleteDir()
  unstash 'source'
  dir("${BASE_DIR}"){
    withGoEnv() {
      body()
    }
  }
}

def publishArtifacts() {
  if(env.IS_BRANCH_AVAILABLE == "true") {
    publishArtifactsDRA(type: env.TYPE)
  } else {
    if (env.TYPE == "snapshot" && !isArm()) {
      publishArtifactsDev()
    } else {
      echo "publishArtifacts: type is not required to be published for this particular branch/PR"
    }
  }
}

def runPackage(def args = [:]) {
  def type = args.type
  def makeGoal = 'release-manager-snapshot'
  if (type.equals('staging')) {
    makeGoal = 'release-manager-release'
  }
  runWithGo() {
    sh(label: "make ${makeGoal}", script: "make ${makeGoal}")
  }
}

def publishArtifactsDev() {
  def bucketLocation = "gs://${JOB_GCS_BUCKET}/pull-requests/pr-${env.CHANGE_ID}"
  if (isPR()) {
    bucketLocation = "gs://${JOB_GCS_BUCKET}/snapshots"
  }
  uploadArtifacts(bucketLocation: bucketLocation)
  uploadArtifacts(bucketLocation: "gs://${JOB_GCS_BUCKET}/${URI_SUFFIX}")

  dockerLogin(secret: env.DOCKER_SECRET, registry: env.DOCKER_REGISTRY)
  dir("${BASE_DIR}"){
    sh(label: 'Push', script: "./.ci/scripts/push-docker.sh ${env.GIT_BASE_COMMIT} ${env.DOCKER_IMAGE}")
  }
}

def publishArtifactsDRA(def args = [:]) {
  uploadArtifacts(bucketLocation: getBucketLocation(args.type))
}

def uploadArtifacts(def args = [:]) {
  // Copy those files to another location with the sha commit to test them afterward.
  googleStorageUpload(bucket: "${args.bucketLocation}",
    credentialsId: "${JOB_GCS_CREDENTIALS}",
    pathPrefix: "${BASE_DIR}/build/distributions/",
    pattern: "${BASE_DIR}/build/distributions/**/*",
    sharedPublicly: true,
    showInline: true)
  // Copy the dependencies files if no ARM
  whenFalse(isArm()) {
    googleStorageUpload(bucket: "${args.bucketLocation}",
      credentialsId: "${JOB_GCS_CREDENTIALS}",
      pathPrefix: "${BASE_DIR}/build/",
      pattern: "${BASE_DIR}/build/dependencies.csv",
      sharedPublicly: true,
      showInline: true)
  }
}

def getBucketLocation(type) {
  return "gs://${JOB_GCS_BUCKET}/${URI_SUFFIX}/${type}"
}

def notifyStatus(def args = [:]) {
  def releaseManagerFile = args.get('file', '')
  def analyse = args.get('analyse', false)
  def subject = args.get('subject', '')
  def body = args.get('body', '')
  releaseManagerNotification(file: releaseManagerFile,
                             analyse: analyse,
                             slackChannel: "${env.SLACK_CHANNEL}",
                             slackColor: 'danger',
                             slackCredentialsId: 'jenkins-slack-integration-token',
                             to: "${env.NOTIFY_TO}",
                             subject: subject,
                             body: "Build: (<${env.RUN_DISPLAY_URL}|here>).\n ${body}")
}

def runIfNoMainAndNoStaging(Closure body) {
  if (env.BRANCH_NAME.equals('main') && env.TYPE == 'staging') {
    echo 'INFO: staging artifacts for the main branch are not required.'
  } else {
    body()
  }
}

/**
* Prepare the context to be able to create the branch, push the changes and create the pull request
*
* NOTE: This particular implementation requires to checkout with the step gitCheckout
*/
def withGitContext(Closure body) {
  setupAPMGitEmail(global: true)
  // get the the workspace for the package-storage repository
  setEnvVar('PACKAGE_STORAGE_LOCATION', sh(label: 'get-package-storage-location', script: 'make --no-print-directory -C .ci/scripts get-package-storage-location', returnStdout: true)?.trim())
  withCredentials([usernamePassword(credentialsId: '2a9602aa-ab9f-4e52-baf3-b71ca88469c7-UserAndToken',
                                    passwordVariable: 'GITHUB_TOKEN', usernameVariable: 'GITHUB_USER')]) {
    try {
      echo("env.PACKAGE_STORAGE_LOCATION=${env.PACKAGE_STORAGE_LOCATION}")
      // within the package-storage workspace then configure the credentials to be able to push the changes
      dir(env.PACKAGE_STORAGE_LOCATION) {
        sh(label: 'List files', script: 'ls -1')
        sh(label: 'Setup git context', script: """git config remote.origin.url "https://${GITHUB_USER}:${GITHUB_TOKEN}@github.com/${ORG_NAME}/package-storage.git" """)
      }
      // run the given body to prepare the changes and push the changes
      withGhEnv(version: '2.4.0') {
        body()
      }
    } finally {
      dir(env.PACKAGE_STORAGE_LOCATION) {
        sh(label: 'Rollback git context', script: """git config remote.origin.url "https://github.com/${ORG_NAME}/package-storage.git" """)
      }
    }
  }
}

def smartGitCheckout() {
  // Checkout the given commit
  if (env.COMMIT?.trim()) {
    gitCheckout(basedir: "${BASE_DIR}",
                branch: "${env.COMMIT}",
                credentialsId: "${JOB_GIT_CREDENTIALS}",
                repo: "https://github.com/elastic/${REPO}.git")
  } else {
    gitCheckout(basedir: "${BASE_DIR}",
                githubNotifyFirstTimeContributor: false,
                shallow: false)
  }
}

def packageStoragePublish(builtPackagesPath) {
  def unpublished = signUnpublishedArtifactsWithElastic(builtPackagesPath)
  if (!unpublished) {
    echo 'Package has been already published'
    return
  }
  uploadUnpublishedToPackageStorage(builtPackagesPath)
}

def signUnpublishedArtifactsWithElastic(builtPackagesPath) {
  def unpublished = false
  dir(builtPackagesPath) {
    findFiles()?.findAll{ it.name.endsWith('.zip') }?.collect{ it.name }?.sort()?.each {
      def packageZip = it
      if (isAlreadyPublished(packageZip)) {
        return
      }

      unpublished = true
      googleStorageUpload(bucket: env.INFRA_SIGNING_BUCKET_ARTIFACTS_PATH,
        credentialsId: env.INTERNAL_CI_JOB_GCS_CREDENTIALS,
        pattern: '*.zip',
        sharedPublicly: false,
        showInline: true)
    }
  }

  if (!unpublished) {
    return unpublished
  }

  withCredentials([string(credentialsId: env.JOB_SIGNING_CREDENTIALS, variable: 'TOKEN')]) {
    triggerRemoteJob(auth: CredentialsAuth(credentials: 'local-readonly-api-token'),
      job: 'https://internal-ci.elastic.co/job/elastic+unified-release+master+sign-artifacts-with-gpg',
      token: TOKEN,
      parameters: "gcs_input_path=${env.INFRA_SIGNING_BUCKET_ARTIFACTS_PATH}",
      useCrumbCache: false,
      useJobInfoCache: false)
  }
  googleStorageDownload(bucketUri: "${env.INFRA_SIGNING_BUCKET_SIGNED_ARTIFACTS_PATH}/*",
    credentialsId: env.INTERNAL_CI_JOB_GCS_CREDENTIALS,
    localDirectory: builtPackagesPath + '/',
    pathPrefix: "${env.INFRA_SIGNING_BUCKET_SIGNED_ARTIFACTS_SUBFOLDER}")
    sh(label: 'Rename .asc to .sig', script: 'for f in ' + builtPackagesPath + '/*.asc; do mv "$f" "${f%.asc}.sig"; done')
  archiveArtifacts(allowEmptyArchive: false, artifacts: "${builtPackagesPath}/*.sig")
  return unpublished
}

def uploadUnpublishedToPackageStorage(builtPackagesPath) {
  def dryRun = env.BRANCH_NAME != 'master'
  if (dryRun) {
    echo "Dry run: endpoint-package won't be published"
  }

  dir(builtPackagesPath) {
    withGCPEnv(secret: env.PACKAGE_STORAGE_UPLOADER_GCP_SERVICE_ACCOUNT) {
      withCredentials([string(credentialsId: env.PACKAGE_STORAGE_UPLOADER_CREDENTIALS, variable: 'TOKEN')]) {
        findFiles()?.findAll{ it.name.endsWith('.zip') }?.collect{ it.name }?.sort()?.each {
          def packageZip = it
          if (isAlreadyPublished(packageZip)) {
            return
          }

          sh(label: 'Upload package .zip file', script: "gsutil cp ${packageZip} ${env.PACKAGE_STORAGE_INTERNAL_BUCKET_QUEUE_PUBLISHING_PATH}/")
          sh(label: 'Upload package .sig file', script: "gsutil cp ${packageZip}.sig ${env.PACKAGE_STORAGE_INTERNAL_BUCKET_QUEUE_PUBLISHING_PATH}/")

          // FIXME legacy_package=false
          // endpoint-package must be aligned with spec first, this option disables validation on the job side
          triggerRemoteJob(auth: CredentialsAuth(credentials: 'local-readonly-api-token'),
            job: 'https://internal-ci.elastic.co/job/package_storage/job/publishing-job-remote',
            token: TOKEN,
            parameters: """
              dry_run=true
              gs_package_build_zip_path=${env.PACKAGE_STORAGE_INTERNAL_BUCKET_QUEUE_PUBLISHING_PATH}/${packageZip}
              gs_package_signature_path=${env.PACKAGE_STORAGE_INTERNAL_BUCKET_QUEUE_PUBLISHING_PATH}/${packageZip}.sig
              legacy_package=true""",
              useCrumbCache: true,
              useJobInfoCache: true)
        }
      }
    }
  }
}

def isAlreadyPublished(packageZip) {
  def responseCode = httpRequest(method: "HEAD",
    url: "https://package-storage.elastic.co/artifacts/packages/${packageZip}",
    response_code_only: true)
  return responseCode == 200
}
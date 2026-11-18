@Library('sandcorp') _

// Deploys the production branch head only (job branch specifier */production).
//
//   main push -> GitHub Actions promote.yml: checks || image (see images below, tagged <sha>) -> production fast-forward
//   production push -> this job: no build, no tests. Waits briefly for the image of that sha, pulls it on the
//                      deploy node, recreates the service, waits for the container health check and runs /health.
//                      If the new container is not healthy it switches back to the previous successful image.
//
// Host-specific values stay out of this public repository:
//   - the deploy node is whatever Jenkins node carries the label below;
//   - secrets and MAIL_BIND_ADDRESS come from the .env file in the job workspace on that node (not created here),
//     or from the node's DEPLOY_ENV_FILE, which the shared library links to ./.env before each deploy.
// The Compose project name, service and volume names must stay as in compose.yaml so the existing data volume is reused.
sandcorpDeploy(
  name: 'Switchboard',
  deployFrom: 'production',
  deployNode: 'switchboard-deploy', // node label on the deploy host
  // image.yml pushes this name (and a copy under the old agent-mail name until phase 3). Images before #157 exist only under
  // the old name, so ROLLBACK_SHA to such a commit cannot pull.
  images: [[repo: 'ghcr.io/sandcorporation/switchboard', envVar: 'SWITCHBOARD_IMAGE']],
  imageWaitHint: 'https://github.com/sandcorporation/switchboard/actions/workflows/promote.yml',
  composeProject: 'switchboard', // owns the data volume switchboard_switchboard_data
  composeFiles: ['compose.yaml'],
  service: 'switchboard', // must match compose.yaml
  healthCmd: 'wget -q -O - http://127.0.0.1:8080/health',
  // Stop before touching the running service if the deploy .env lacks these. Without MAIL_BIND_ADDRESS Compose
  // would silently bind to loopback and every remote client would lose the mailbox.
  // The .env may live outside the workspace: set DEPLOY_ENV_FILE on the deploy node (shared library README).
  requiredEnv: ['SWITCHBOARD_TOKENS', 'MAIL_BIND_ADDRESS'],
)

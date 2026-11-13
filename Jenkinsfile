@Library('sandcorp') _

// Deploys the production branch head only (job branch specifier */production).
//
//   main push -> GitHub Actions promote.yml: checks || image (ghcr.io/sandcorporation/agent-mail:<sha>) -> production fast-forward
//   production push -> this job: no build, no tests. Waits briefly for the image of that sha, pulls it on the
//                      deploy node, recreates the service, waits for the container health check and runs /health.
//                      If the new container is not healthy it switches back to the previous successful image.
//
// Host-specific values stay out of this public repository:
//   - the deploy node is whatever Jenkins node carries the label below;
//   - secrets and MAIL_BIND_ADDRESS come from the .env file in the job workspace on that node (not created here).
// The Compose project name, service and volume names must stay as in compose.yaml so the existing data volume is reused.
sandcorpDeploy(
  name: 'Agent Mail',
  deployFrom: 'production',
  deployNode: 'agent-mail-deploy',
  images: [[repo: 'ghcr.io/sandcorporation/agent-mail', envVar: 'AGENT_MAIL_IMAGE']],
  imageWaitHint: 'https://github.com/sandcorporation/agent-mail/actions/workflows/promote.yml',
  composeProject: 'agent-mail',
  composeFiles: ['compose.yaml'],
  service: 'agent-mail',
  healthCmd: 'wget -q -O - http://127.0.0.1:8080/health',
)

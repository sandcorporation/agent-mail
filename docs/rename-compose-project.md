# Rename the Compose project

One-time move of the deployment from the `agent-mail` names to the `switchboard` names, on the deploy host.

| | Before | After |
| --- | --- | --- |
| Compose project | `agent-mail` | `switchboard` |
| service | `agent-mail` | `switchboard` |
| data volume | `agent-mail_agent_mail_data` | `switchboard_switchboard_data` |
| container | `agent-mail-agent-mail-1` | `switchboard-switchboard-1` |
| Jenkins job | `agent-mail` | `switchboard` |

`scripts/rename-compose-project.sh` stops the old container, copies the volume to the new name, verifies the copy, takes a SQLite backup and starts the new project with the image the old container ran. On any failure it starts the old container again and exits non-zero. It never removes the old volume or the old container. `--help` lists the options; the defaults are the names above.

## Order

The script must run before the Jenkinsfile with the new names deploys: a deploy of the new project without the moved volume would start an empty database next to the old container.

1. Disable the `agent-mail` Jenkins job, so a `production` push cannot recreate the old project while the move runs.
2. On the deploy host, get the new `compose.yaml` and the script (from this change, before merging it), and run from the old job workspace:

   ```sh
   git -C <old workspace> fetch origin <branch>
   git -C <old workspace> show origin/<branch>:compose.yaml > /tmp/compose.switchboard.yaml
   git -C <old workspace> show origin/<branch>:scripts/rename-compose-project.sh > /tmp/rename-compose-project.sh
   cd <old workspace>
   bash /tmp/rename-compose-project.sh --compose-file /tmp/compose.switchboard.yaml --dry-run
   bash /tmp/rename-compose-project.sh --compose-file /tmp/compose.switchboard.yaml --backup-dir "$BACKUP"
   ```

   The env file defaults to `./.env` (the job workspace `.env`, or the link to `DEPLOY_ENV_FILE`). It is passed to Compose and never printed.
3. Create the `switchboard` Jenkins job: same configuration (pipeline from `Jenkinsfile`, branch `*/production`, `ROLLBACK_SHA` parameter). The workspace on the node follows the job name, so the `.env` does not move with it:
   - with `DEPLOY_ENV_FILE` set on the node, nothing to do;
   - otherwise copy it into the new workspace, mode `0600`, before the first build: `install -D -m 600 <old workspace>/.env <new workspace>/.env`. A first build without it stops at the `requiredEnv` check before touching the running service.
4. Merge this change. `promote` moves `production`; the `switchboard` job recreates the `switchboard` service with that image on the moved volume.
5. Check `/health`, a client `inbox`, and the dashboard. Then delete the `agent-mail` job and remove the stopped old container (`docker rm agent-mail-agent-mail-1`). Keep `agent-mail_agent_mail_data` until the backups of the new volume have run for a while, then remove it by hand.

Point backup jobs that name the old volume or container at the new names.

## Undo

Before step 4: `docker compose -p switchboard -f /tmp/compose.switchboard.yaml --env-file .env stop`, `docker start agent-mail-agent-mail-1`, enable the `agent-mail` job. Mail written to the new project after the move is only in `switchboard_switchboard_data`.

## Downtime

From the stop to the new container's health check: about 10 s plus the copy and verification, which read the database about five times. Measured on a local disk: 9 s for a 1 MB database, 29 s for 500 MB.

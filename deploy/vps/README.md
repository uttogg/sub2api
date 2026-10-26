# VPS deploy

`deploy.py` pulls a published sub2api image and switches the single production host over to it. It is the parameterized replacement for the one-off blue-green script. Python 3 standard library only. Run it as root on the VPS.

```bash
python3 /opt/sub2api-deploy/deploy.py --image ghcr.io/uttogg/sub2api:latest
```

Copy this file to `/opt/sub2api-deploy/deploy.py` after it lands on `main`. A sha tag works the same way, for example `--image ghcr.io/uttogg/sub2api:<full-commit-sha>`. The default image is `ghcr.io/uttogg/sub2api:latest`, and the default deploy directory is `/opt/sub2api-deploy`.

`--dry-run` prints the plan and does not pull, rewrite Nginx, start a container, or take the lock.

## What a deploy does

1. Takes `/opt/sub2api-deploy/.deployment.lock`. A second run exits if the lock is held.
2. Reads the running `sub2api` container, its image, and its `org.opencontainers.image.revision` label. Postgres and Redis are left in place.
3. `docker pull`s the requested image. If that revision is already the running image, it prints `already up to date` and exits 0.
4. Writes a `pg_dump` custom-format backup under `/opt/sub2api-deploy/releases/release-<timestamp>-<revision>/` and keeps the 3 newest release directories.
5. Starts `sub2api-candidate-<timestamp>` on `127.0.0.1:18081` with a compose override that inherits the live `sub2api` service, including its env, networks, and mounts.
6. Waits until the candidate is healthy, its `--version` matches the image labels, and its logs contain no schema-migration errors. `schema_migrations` must not shrink.
7. Switches Nginx `proxy_pass` from `127.0.0.1:8080` to `127.0.0.1:18081`, runs `nginx -t`, reloads, and probes `/health` (HTTP 200, `status=ok`) and `/v1/models` (HTTP 401, `code=API_KEY_REQUIRED`) through `curl --resolve`.
8. Drains the old container. The default wait is 30 minutes (`--drain-timeout 30m`). When the timeout is reached the deploy proceeds and records `drain_timeout`.
9. Updates the `sub2api` image line in `docker-compose.yml` and runs `docker compose up -d --no-deps sub2api`. If Compose does not recreate onto the new image id, the same command is repeated with `--force-recreate`.
10. Switches Nginx back to `127.0.0.1:8080`, drains and removes the candidate, and deletes sub2api images other than the one just deployed and the previous one.
11. Writes `/opt/sub2api-deploy/deploy-state.json` and prints a final `STATUS` line.

`sub.520ikun.xyz` is required. `sub.050213.xyz` is optional: a missing vhost file, a 404, or a probe that does not answer is a warning and does not fail the deploy.

## Failure

Before the new container is serving on port 8080, a failure restores the compose file, retags the previous image, recreates the previous container, reloads Nginx onto it, and removes the candidate. The database dump is not restored automatically. After the new container is already serving, a cleanup error does not switch traffic back; the status line is `STATUS deployed cleanup incomplete`.

The candidate applies migrations to the shared database before traffic moves. Keep the release directory until the new version is known to be good.

## Registry login

`ghcr.io/uttogg/sub2api` is a public package, so the VPS can `docker pull` it with no token. If the package is later made private, log in once as the user that runs the deploy. Use a classic PAT, or a fine-grained token, with `read:packages` on `uttogg/sub2api`:

```bash
umask 077
mkdir -p /root/.config
editor /root/.config/ghcr-token
docker login ghcr.io -u uttogg --password-stdin < /root/.config/ghcr-token
```

`docker login` stores credentials in the root Docker config. Later pulls from cron reuse them. The deploy script never takes the token on the command line.

A daily run, once the script is installed:

```cron
15 4 * * * root python3 /opt/sub2api-deploy/deploy.py --image ghcr.io/uttogg/sub2api:latest >> /var/log/sub2api-deploy.log 2>&1
```

Images are built by `.github/workflows/production-image.yml` on every push to `main` and from Actions → Production image → Run workflow.

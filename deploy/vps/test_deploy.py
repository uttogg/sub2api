#!/usr/bin/env python3
"""Unit tests for the VPS deploy helper. They do not need Docker or Nginx."""

import io
import json
import pathlib
import tempfile
import unittest
from contextlib import redirect_stdout

import deploy


COMPOSE = """
services:
  sub2api:
    image: sub2api:local-0.2.17
    container_name: sub2api
    ports:
      - "127.0.0.1:8080:8080"
  postgres:
    image: postgres:18-alpine
    container_name: sub2api-postgres
"""


class DeployHelperTest(unittest.TestCase):
    def test_duration_parser(self):
        self.assertEqual(deploy.parse_duration("30"), 30)
        self.assertEqual(deploy.parse_duration("30s"), 30)
        self.assertEqual(deploy.parse_duration("30m"), 1800)
        self.assertEqual(deploy.parse_duration("2h"), 7200)
        with self.assertRaises(Exception):
            deploy.parse_duration("0s")
        with self.assertRaises(Exception):
            deploy.parse_duration("soon")

    def test_image_repository(self):
        self.assertEqual(
            deploy.image_repository("ghcr.io/uttogg/sub2api:latest"),
            "ghcr.io/uttogg/sub2api",
        )
        self.assertEqual(
            deploy.image_repository("ghcr.io/uttogg/sub2api@sha256:abcd"),
            "ghcr.io/uttogg/sub2api",
        )
        self.assertEqual(
            deploy.image_repository("sub2api:local-0.2.17-0353f003"),
            "sub2api",
        )
        self.assertEqual(
            deploy.image_repository("localhost:5000/sub2api:tag"),
            "localhost:5000/sub2api",
        )

    def test_same_revision_by_label_or_id(self):
        running = {
            "Image": "sha256:aaa",
            "Config": {"Labels": {deploy.REVISION_LABEL: "a" * 40}},
        }
        pulled = {
            "Id": "sha256:bbb",
            "Config": {"Labels": {deploy.REVISION_LABEL: "a" * 40}},
        }
        self.assertTrue(deploy.same_revision(running, pulled))
        pulled["Config"]["Labels"][deploy.REVISION_LABEL] = "b" * 40
        self.assertFalse(deploy.same_revision(running, pulled))
        running["Image"] = "sha256:bbb"
        pulled["Id"] = "sha256:bbb"
        running["Config"]["Labels"] = {}
        pulled["Config"]["Labels"] = {}
        self.assertTrue(deploy.same_revision(running, pulled))

    def test_replace_service_image_keeps_other_services(self):
        updated, old = deploy.replace_service_image(
            COMPOSE, "sub2api", "ghcr.io/uttogg/sub2api:abc"
        )
        self.assertEqual(old, "sub2api:local-0.2.17")
        self.assertIn('image: "ghcr.io/uttogg/sub2api:abc"', updated)
        self.assertIn("image: postgres:18-alpine", updated)
        self.assertNotIn("image: sub2api:local-0.2.17", updated)

    def test_rewrite_proxy_pass_accepts_either_port(self):
        text = "proxy_pass http://127.0.0.1:8080;\n"
        switched = deploy.rewrite_proxy_pass(text, 8080, 18081, 18081)
        self.assertEqual(switched, "proxy_pass http://127.0.0.1:18081;\n")
        restored = deploy.rewrite_proxy_pass(switched, 8080, 18081, 8080)
        self.assertEqual(restored, "proxy_pass http://127.0.0.1:8080;\n")
        with self.assertRaises(deploy.DeployError):
            deploy.rewrite_proxy_pass(text + text, 8080, 18081, 18081)

    def test_optional_vhost_parse(self):
        args = deploy.parse_args([])
        roles = {item["domain"]: item["optional"] for item in args.vhosts}
        self.assertFalse(roles["sub.520ikun.xyz"])
        self.assertTrue(roles["sub.050213.xyz"])

    def test_migration_errors_ignore_setting_warnings(self):
        logs = "\n".join(
            [
                "Auto setup mode enabled...",
                "Warning: migrate openai allow Claude Code Codex plugin setting failed: boom",
                "server listening",
            ]
        )
        self.assertEqual(deploy.migration_error_lines(logs), [])
        failed = logs + "\nAuto setup failed: apply migration 245_example.sql: duplicate"
        found = deploy.migration_error_lines(failed)
        self.assertEqual(len(found), 1)
        self.assertIn("apply migration", found[0])

    def test_version_text_and_contract(self):
        text = (
            "2026/10/26 04:50:00 Sub2API 0.2.17 "
            "(commit: 0353f0032b0669a6344303f32d166d9f82f32b3e, built: now)"
        )
        version, commit = deploy.parse_version_text(text)
        self.assertEqual(version, "0.2.17")
        self.assertEqual(commit, "0353f0032b0669a6344303f32d166d9f82f32b3e")
        info = {
            "Os": "linux",
            "Architecture": "amd64",
            "Config": {
                "Entrypoint": ["/app/docker-entrypoint.sh"],
                "Cmd": ["/app/sub2api"],
                "WorkingDir": "/app",
                "Healthcheck": {"Test": ["CMD", "wget", "http://localhost:8080/health"]},
            },
        }
        self.assertEqual(deploy.runtime_contract_errors(info), [])
        info["Architecture"] = "arm64"
        self.assertTrue(deploy.runtime_contract_errors(info))

    def test_connection_counts_exclude_infrastructure(self):
        listing = "\n".join(
            [
                "ESTAB 0 0 172.18.0.5:8080 10.0.0.8:44321",
                "ESTAB 0 0 127.0.0.1:8080 127.0.0.1:12345",
                "ESTAB 0 0 172.18.0.5:40000 172.18.0.2:5432",
                "TIME-WAIT 0 0 172.18.0.5:8080 10.0.0.9:1111",
                "ESTAB 0 0 172.18.0.5:55555 127.0.0.11:53",
                "SYN-SENT 0 0 172.18.0.5:55556 1.2.3.4:443",
            ]
        )
        inbound, upstream, _details = deploy.connection_counts(
            listing, {"172.18.0.2:5432"}
        )
        self.assertEqual(inbound, 2)
        self.assertEqual(upstream, 1)

    def test_release_and_image_retention(self):
        entries = [
            ("release-20261026T010000Z-aaa", 1),
            ("release-20261026T020000Z-bbb", 2),
            ("release-20261026T030000Z-ccc", 3),
            ("release-20261026T040000Z-ddd", 4),
            ("notes", 5),
        ]
        self.assertEqual(
            deploy.releases_to_delete(entries, 3, "release-20261026T040000Z-ddd"),
            ["release-20261026T010000Z-aaa"],
        )
        rows = [
            ("sha256:new", "ghcr.io/uttogg/sub2api", "latest"),
            ("sha256:prev", "ghcr.io/uttogg/sub2api", "old"),
            ("sha256:ancient", "ghcr.io/uttogg/sub2api", "older"),
            ("sha256:localold", "sub2api", "local"),
            ("sha256:db", "postgres", "18-alpine"),
        ]
        removed = deploy.images_to_remove(
            rows,
            ["sha256:new", "sha256:prev"],
            {"ghcr.io/uttogg/sub2api", "sub2api"},
        )
        self.assertEqual(
            {(repo, tag) for _image, repo, tag in removed},
            {("ghcr.io/uttogg/sub2api", "older"), ("sub2api", "local")},
        )

    def test_rollback_decisions(self):
        self.assertEqual(
            deploy.decide_rollback({"canonical_serving": True, "compose_updated": True}),
            "keep_canonical",
        )
        self.assertEqual(
            deploy.decide_rollback({"compose_updated": True, "old_healthy": True}),
            "restore_old",
        )
        self.assertEqual(
            deploy.decide_rollback({"nginx_on_candidate": True, "old_healthy": True}),
            "revert_nginx",
        )
        self.assertEqual(
            deploy.decide_rollback({"nginx_on_candidate": True, "old_healthy": False}),
            "retain_candidate",
        )
        self.assertEqual(
            deploy.decide_rollback({"candidate_started": True}),
            "remove_candidate",
        )
        self.assertEqual(deploy.decide_rollback({}), "none")

    def test_probe_evaluation(self):
        self.assertIsNone(
            deploy.evaluate_probe(
                "sub.520ikun.xyz", "/health", 200, json.dumps({"status": "ok"})
            )
        )
        self.assertIsNone(
            deploy.evaluate_probe(
                "sub.520ikun.xyz",
                "/v1/models",
                401,
                json.dumps({"code": "API_KEY_REQUIRED"}),
            )
        )
        problem = deploy.evaluate_probe("sub.050213.xyz", "/health", 404, "missing")
        self.assertIn("404", problem)

    def test_candidate_compose_pins_live_mounts_and_networks(self):
        args = deploy.parse_args(["--image", "ghcr.io/uttogg/sub2api:abc"])
        text = deploy.render_candidate_compose(
            args,
            args.image,
            "sub2api-candidate-1",
            [("sub2api-network", "sub2api-deploy_sub2api-network")],
            [
                {
                    "Type": "volume",
                    "Name": "sub2api-deploy_sub2api_data",
                    "Destination": "/app/data",
                    "RW": True,
                }
            ],
        )
        self.assertIn("image: \"ghcr.io/uttogg/sub2api:abc\"", text)
        self.assertIn("127.0.0.1:18081:8080", text)
        self.assertIn("external: true", text)
        self.assertIn("name: sub2api-deploy_sub2api-network", text)
        self.assertIn("name: sub2api-deploy_sub2api_data", text)
        self.assertIn("depends_on: !reset {}", text)

    def test_dry_run_makes_no_changes_and_prints_the_plan(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            stdout = io.StringIO()
            with redirect_stdout(stdout):
                code = deploy.main(
                    [
                        "--dry-run",
                        "--deploy-dir",
                        str(root),
                        "--image",
                        "ghcr.io/uttogg/sub2api:latest",
                    ]
                )
            self.assertEqual(code, 0)
            output = stdout.getvalue()
            self.assertIn("dry-run: no changes will be made", output)
            self.assertIn("ghcr.io/uttogg/sub2api:latest", output)
            self.assertIn("migration errors", output)
            self.assertNotIn("\nalready up to date\n", "\n" + output + "\n")
            self.assertFalse((root / "deploy-state.json").exists())
            self.assertFalse((root / ".deployment.lock").exists())


if __name__ == "__main__":
    unittest.main()

"""Exercise release and tag-sync safeguards using disposable local Git repos."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPTS = Path(__file__).resolve().parents[1]


class ReleaseScripts(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name) / "repo"
        self.repo.mkdir()
        self.env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
        for key in ("GITHUB_REF", "GITHUB_ENV", "GITHUB_REPOSITORY", "GH_REPO", "VERSION"):
            self.env.pop(key, None)
        self.git("init", "-b", "main")
        self.git("config", "user.name", "Release tests")
        self.git("config", "user.email", "release-tests@example.invalid")
        self.commit("upstream")
        self.git("tag", "-a", "v1.14.4", "-m", "upstream")
        self.commit("custom")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")

    def run_command(self, *args, success=True, env=None):
        result = subprocess.run(args, cwd=self.repo, env=env or self.env,
                                text=True, capture_output=True)
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout.strip()

    def git(self, *args):
        return self.run_command("git", *args)

    def commit(self, message):
        self.git("commit", "--allow-empty", "-m", message)

    def script(self, name, *args, **kwargs):
        return self.run_command("bash", str(SCRIPTS / name), *args, **kwargs)

    def custom_tag(self, name="v1.14.4-custom"):
        self.git("tag", "-a", name, "-m", name)

    def test_daily_build_retains_custom_suffix_and_commit(self):
        self.assertRegex(self.script("custom_version.sh"),
                         r"^1\.14\.4-dev\.1\.g[0-9a-f]{12}-custom$")

    def test_annotated_release_preserves_revision(self):
        self.custom_tag("v1.14.4-2-custom")
        self.assertEqual(self.script("custom_version.sh", "--release", "1.14.4-2-custom"),
                         "1.14.4-2-custom")

    def test_upstream_and_missing_release_tags_are_rejected(self):
        for version in ("v1.14.4", "v1.14.4-custom", "v1.14.4-0-custom"):
            with self.subTest(version=version):
                self.script("custom_version.sh", "--release", version, success=False)
        self.script("custom_version.sh", "--release", success=False)

    def test_release_outside_main_is_rejected(self):
        self.git("switch", "-c", "feature/unmerged")
        self.commit("unmerged")
        self.custom_tag()
        self.script("custom_version.sh", "--release", "v1.14.4-custom", success=False)

    def test_old_release_can_be_built_without_building_main_tip(self):
        self.custom_tag()
        release_commit = self.git("rev-parse", "HEAD")
        self.commit("later main commit")
        self.git("update-ref", "refs/remotes/origin/main", "HEAD")
        self.script("custom_version.sh", "--release", "v1.14.4-custom", success=False)
        self.git("checkout", "--detach", release_commit)
        self.assertEqual(self.script("custom_version.sh", "--release", "v1.14.4-custom"),
                         "1.14.4-custom")

    def test_existing_tag_cannot_be_moved(self):
        self.custom_tag()
        original = self.git("rev-parse", "refs/tags/v1.14.4-custom")
        self.script("set_custom_tag.sh", "1.14.4-custom")
        self.commit("later commit")
        self.script("set_custom_tag.sh", "1.14.4-custom", success=False)
        self.assertEqual(self.git("rev-parse", "refs/tags/v1.14.4-custom"), original)

    def test_github_tag_event_does_not_lose_custom_suffix(self):
        self.custom_tag()
        env = dict(self.env, GITHUB_REF="refs/tags/v1.14.4-custom")
        self.assertEqual(self.script("custom_version.sh", "--release", env=env), "1.14.4-custom")

    def test_daily_version_ignores_ancestor_custom_tags(self):
        self.custom_tag("v1.14.4-2-custom")
        self.commit("after release")
        self.assertRegex(self.script("custom_version.sh"),
                         r"^1\.14\.4-dev\.2\.g[0-9a-f]{12}-custom$")

    def test_local_build_tag_is_readable_but_cannot_be_released(self):
        version = self.script("custom_version.sh")
        self.script("set_custom_tag.sh", version)
        self.assertEqual(self.script("custom_version.sh"), version)
        self.script("custom_version.sh", "--release", version, success=False)

    def mock_github(self, state):
        directory = Path(self.temp.name) / "bin"
        directory.mkdir()
        gh = directory / "gh"
        gh.write_text('''#!/usr/bin/env bash
set -eu
printf '%s:%s\\n' "$GH_REPO" "$*" >> "$GH_TEST_LOG"
if [[ "$1 $2" == 'release view' ]]; then
  [[ "$GH_TEST_STATE" != missing ]] || exit 1
  printf '%s\\n' "$GH_TEST_STATE"
fi
''')
        gh.chmod(0o755)
        log = Path(self.temp.name) / "gh-log"
        env = dict(self.env, PATH=str(directory) + os.pathsep + self.env["PATH"],
                   GH_TEST_STATE=state, GH_TEST_LOG=str(log))
        self.git("remote", "add", "origin", "git@github.com:qiudesong/sing-box.git")
        self.git("remote", "add", "upstream", "https://github.com/SagerNet/sing-box.git")
        self.custom_tag()
        artifact = self.repo / "sing-box.tar.gz"
        artifact.write_bytes(b"test artifact")
        return env, log, artifact

    def test_publish_creates_stable_draft_in_fork_not_upstream(self):
        env, log, artifact = self.mock_github("missing")
        self.script("upload_custom_release.sh", "1.14.4-custom", str(artifact), env=env)
        calls = log.read_text().splitlines()
        self.assertEqual(len(calls), 3)
        self.assertTrue(all(call.startswith("qiudesong/sing-box:") for call in calls))
        self.assertIn("release create v1.14.4-custom --verify-tag --draft", calls[1])
        self.assertIn("--prerelease=false", calls[1])
        self.assertIn("release upload v1.14.4-custom", calls[2])

    def test_publish_refuses_to_replace_published_release(self):
        env, log, artifact = self.mock_github("false")
        self.script("upload_custom_release.sh", "1.14.4-custom", str(artifact),
                    env=env, success=False)
        self.assertEqual(len(log.read_text().splitlines()), 1)

    def test_apk_versions(self):
        for version, expected in {
            "1.14.4-custom": "1.14.4_p1-r0",
            "1.14.4-2-custom": "1.14.4_p2-r0",
            "1.15.0-alpha.13-custom": "1.15.0_alpha13_p1-r0",
            "1.14.4-dev.12.gabcdef123456-custom": "1.14.4_git12_p1-r0",
            "1.15.0-beta.2-dev.3.gabcdef123456-custom": "1.15.0_beta2_git3_p1-r0",
        }.items():
            with self.subTest(version=version):
                self.assertEqual(self.script("apk_version.sh", version), expected)
        self.script("apk_version.sh", "1.14.4", success=False)

    def test_release_channels(self):
        for version, track, docker in (
            ("1.14.4-custom", "custom", "latest-custom"),
            ("1.14.4-2-custom", "custom", "latest-custom"),
            ("1.15.0-rc.1-custom", "custom-beta", "latest-custom-beta"),
            ("1.14.4-dev.2.gabcdef123456-custom", "custom-testing", "edge-custom"),
        ):
            with self.subTest(version=version):
                output = Path(self.temp.name) / "github-env"
                output.write_text("")
                self.script("detect_track.sh", env=dict(self.env, VERSION=version,
                                                       GITHUB_ENV=str(output)))
                self.assertEqual(output.read_text(),
                                 f"TRACK={track}\nNAME=sing-box-custom\nDOCKER_TAG={docker}\n")
        self.script("detect_track.sh", env=dict(self.env, VERSION="1.14.4"), success=False)

    def prepare_sync(self):
        origin = Path(self.temp.name) / "origin.git"
        self.git("clone", "--bare", str(self.repo), str(origin))
        self.git("remote", "add", "origin", str(origin))
        source = self.repo
        checkout = Path(self.temp.name) / "checkout"
        self.git("clone", str(origin), str(checkout))
        return source, checkout, origin

    def test_sync_imports_upstream_tags_and_preserves_custom_tags(self):
        self.custom_tag()
        custom_object = self.git("rev-parse", "refs/tags/v1.14.4-custom")
        source, checkout, origin = self.prepare_sync()
        self.commit("new upstream")
        self.git("tag", "-a", "v1.14.5", "-m", "next upstream")
        upstream_object = self.git("rev-parse", "refs/tags/v1.14.5")
        self.git("tag", "v9.9.9-custom")
        self.repo = checkout
        self.script("sync_upstream_tags.sh", str(source))
        self.script("sync_upstream_tags.sh", str(source))  # idempotent
        refs = self.git("ls-remote", "--tags", "--refs", str(origin))
        self.assertIn(f"{upstream_object}\trefs/tags/v1.14.5", refs)
        self.assertIn(f"{custom_object}\trefs/tags/v1.14.4-custom", refs)
        self.assertNotIn("v9.9.9-custom", refs)

    def test_sync_rejects_rewritten_upstream_tag_without_partial_push(self):
        source, checkout, origin = self.prepare_sync()
        before = self.git("ls-remote", "--tags", "--refs", str(origin))
        self.commit("rewritten upstream")
        self.git("tag", "--force", "v1.14.4")
        self.git("tag", "v1.14.3")  # sorts before the conflicting tag
        self.repo = checkout
        self.script("sync_upstream_tags.sh", str(source), success=False)
        self.assertEqual(self.git("ls-remote", "--tags", "--refs", str(origin)), before)


if __name__ == "__main__":
    unittest.main()

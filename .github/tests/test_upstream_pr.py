#!/usr/bin/env python3
"""Offline integration tests: bare origin, real merges, fake gh (no GitHub writes)."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "update_upstream_pr.sh"


class UpstreamPRTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="upstream-pr-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.origin = self.root / "origin.git"
        self.repo = self.root / "work"
        self.git("init", "--bare", str(self.origin), cwd=self.root)
        self.git("clone", str(self.origin), str(self.repo), cwd=self.root)
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.invalid")
        self.git("switch", "-c", "main")
        self.commit("shared", "base\n")
        self.git("branch", "stable")
        self.git("push", "origin", "main", "stable")
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        fake = bin_dir / "gh"
        fake.write_text(f"#!{sys.executable}\n" + """import os,sys,pathlib
args=sys.argv[1:]
if args[:2] == ['pr', 'list']:
    print(os.environ.get('EXISTING_PR', ''))
else:
    body=pathlib.Path(args[args.index('--body-file')+1]).read_text()
    pathlib.Path(os.environ['PR_BODY']).write_text(body)
""")
        fake.chmod(0o755)
        self.env = dict(os.environ, PATH=str(bin_dir) + os.pathsep + os.environ["PATH"],
                        GH_TOKEN="offline-test", GITHUB_REPOSITORY="test/test",
                        PR_BODY=str(self.root / "body"))

    def git(self, *args, cwd=None):
        return subprocess.run(["git", *args], cwd=cwd or self.repo, text=True,
                              check=True, capture_output=True).stdout.strip()

    def commit(self, path, contents):
        (self.repo / path).write_text(contents)
        self.git("add", path)
        self.git("commit", "-m", "update " + path)
        return self.git("rev-parse", "HEAD")

    def run_update(self):
        # An Actions run starts in a fresh checkout, without local maintenance branches.
        run = self.root / "runner"
        self.git("clone", "--branch", "main", str(self.origin), str(run), cwd=self.root)
        subprocess.run(["bash", str(SCRIPT)], cwd=run, env=self.env, text=True,
                       check=True, capture_output=True)
        self.git("fetch", "origin")

    def remote_tip(self):
        return self.git("rev-parse", "origin/feature/upstream-stable")

    def test_initial_pr_does_not_change_main_or_stable(self):
        main = self.git("rev-parse", "main")
        self.git("switch", "stable")
        stable = self.commit("upstream", "new\n")
        self.git("push", "origin", "stable")
        self.run_update()
        self.assertEqual(stable, self.remote_tip())
        self.assertEqual(main, self.git("rev-parse", "origin/main"))
        self.assertEqual(stable, self.git("rev-parse", "origin/stable"))
        self.assertIn(stable, (self.root / "body").read_text())

    def test_preserves_human_commits(self):
        self.git("switch", "-c", "feature/upstream-stable", "stable")
        human = self.commit("human", "manual resolution\n")
        self.git("push", "origin", "feature/upstream-stable")
        self.git("switch", "main")
        self.commit("custom", "custom behavior\n")
        self.git("push", "origin", "main")
        self.git("switch", "stable")
        self.commit("upstream", "upstream change\n")
        self.git("push", "origin", "stable")
        self.env["EXISTING_PR"] = "42"
        self.run_update()
        self.git("merge-base", "--is-ancestor", human, self.remote_tip())
        self.git("merge-base", "--is-ancestor", "origin/main", self.remote_tip())
        self.git("merge-base", "--is-ancestor", "origin/stable", self.remote_tip())
        self.assertEqual("manual resolution", self.git("show", self.remote_tip() + ":human"))

    def test_conflict_does_not_push_partial_merge(self):
        self.git("switch", "-c", "feature/upstream-stable", "stable")
        human = self.commit("shared", "human resolution\n")
        self.git("push", "origin", "feature/upstream-stable")
        self.git("switch", "main")
        self.commit("shared", "custom conflict\n")
        self.git("push", "origin", "main")
        self.git("switch", "stable")
        self.commit("upstream", "new\n")
        self.git("push", "origin", "stable")
        self.env["EXISTING_PR"] = "42"
        self.run_update()
        self.assertEqual(human, self.remote_tip())
        self.assertIn("merge conflict", (self.root / "body").read_text())

    def test_no_update_is_noop(self):
        self.run_update()
        self.assertFalse((self.root / "body").exists())
        self.assertEqual("", self.git("ls-remote", "--heads", "origin", "feature/upstream-stable"))


if __name__ == "__main__":
    unittest.main()

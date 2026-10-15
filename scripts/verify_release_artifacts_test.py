#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/verify_release_artifacts_test.py —— verify_release_artifacts.py 的 smoke 测试。

覆盖纯函数（sha256_bytes / git_show / git_tree）与 main() 全绿路径；
发布物清单内容本身由 make verify-release-artifacts 做 49 项全量复核。
"""
import contextlib
import hashlib
import importlib.util
import io
import os
import pathlib
import subprocess
import unittest

SPEC = importlib.util.spec_from_file_location(
    "verify_release_artifacts",
    os.path.join(os.path.dirname(__file__), "verify_release_artifacts.py"),
)
VRA = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VRA)


class TestReleaseArtifactVerifierHelpers(unittest.TestCase):
    def test_sha256_bytes_known_vector(self):
        got = VRA.sha256_bytes(b"abc")
        want = hashlib.sha256(b"abc").hexdigest()
        self.assertEqual(got, want)

    def test_git_show_reads_repo_file(self):
        raw = VRA.git_show("HEAD", "go.mod")
        self.assertTrue(raw.startswith(b"module "))
        self.assertEqual(VRA.sha256_bytes(raw), VRA.sha256_bytes(
            pathlib.Path("go.mod").read_bytes()))

    def test_git_tree_returns_sha1(self):
        got = VRA.git_tree("HEAD", "internal/domain")
        self.assertRegex(got, r"^[0-9a-f]{40}$")

    def test_main_release_manifest_all_green(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = VRA.main()
        self.assertEqual(code, 0)
        self.assertIn("项匹配，0 项失败", out.getvalue())

    def test_fail_prints_fail_tag(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            VRA.fail("boom")
        self.assertIn("[FAIL] boom", out.getvalue())


if __name__ == "__main__":
    unittest.main()

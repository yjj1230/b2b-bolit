#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/verify_baseline_candidate_test.py —— baseline candidate 校验器 smoke。"""
import contextlib
import hashlib
import importlib.util
import io
import json
import os
import unittest

SPEC = importlib.util.spec_from_file_location(
    "verify_baseline_candidate",
    os.path.join(os.path.dirname(__file__), "verify_baseline_candidate.py"),
)
VRA = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VRA)


class TestVerifyBaselineCandidateSmoke(unittest.TestCase):
    def test_canon_and_sha_deterministic(self):
        a = VRA.canon({"b": 1, "a": [1, 2]})
        b = VRA.canon({"a": [1, 2], "b": 1})
        self.assertEqual(a, b)
        self.assertEqual(VRA.sha({"x": "y"}),
                         "sha256:" + hashlib.sha256(VRA.canon({"x": "y"}).encode()).hexdigest())

    def test_main_draft_pass(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = VRA.main()
        self.assertEqual(code, 0)
        self.assertIn("publishable=false", out.getvalue())


if __name__ == "__main__":
    unittest.main()

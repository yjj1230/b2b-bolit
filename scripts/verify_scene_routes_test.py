#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""scripts/verify_scene_routes_test.py —— verify_scene_routes.py smoke 测试。"""
import contextlib
import importlib.util
import io
import os
import unittest

SPEC = importlib.util.spec_from_file_location(
    "verify_scene_routes",
    os.path.join(os.path.dirname(__file__), "verify_scene_routes.py"),
)
VRA = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VRA)


class TestVerifySceneRoutesSmoke(unittest.TestCase):
    def test_load_source_contains_api_routes(self):
        src = VRA.load_source(["internal/api"])
        self.assertIn("GET /api/v1/orders", src)
        self.assertIn("SignalPriceReview", src)

    def test_main_all_green(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = VRA.main()
        self.assertEqual(code, 0)
        self.assertIn("33 probes", out.getvalue())


if __name__ == "__main__":
    unittest.main()

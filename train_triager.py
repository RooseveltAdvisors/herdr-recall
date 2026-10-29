#!/usr/bin/env python3
"""Train a small LightGBM ranker and write a tree file the picker can score."""

import json
import sys

import lightgbm as lgb
import numpy as np


def node_from(raw):
    if "leaf_value" in raw:
        return {"leaf": raw["leaf_value"]}
    return {
        "feat": int(raw["split_feature"]),
        "thr": float(raw["threshold"]),
        "left": node_from(raw["left_child"]),
        "right": node_from(raw["right_child"]),
    }


def main():
    src, dst = sys.argv[1], sys.argv[2]
    rows = json.load(open(src))["rows"]
    names = sorted({f for row in rows for f in row["feats"]})
    index = {name: i for i, name in enumerate(names)}
    x = np.zeros((len(rows), len(names)))
    y = np.array([row["y"] for row in rows], dtype=float)
    for i, row in enumerate(rows):
        for feat, val in row["feats"].items():
            x[i, index[feat]] = val
    cut = max(8, int(len(rows) * 0.8))
    train = lgb.Dataset(x[:cut], label=y[:cut])
    model = lgb.train(
        {
            "objective": "regression",
            "num_leaves": 4,
            "min_data_in_leaf": 6,
            "learning_rate": 0.05,
            "feature_fraction": 0.8,
            "bagging_fraction": 0.9,
            "bagging_freq": 1,
            "verbose": -1,
        },
        train,
        num_boost_round=60,
    )
    pred = model.predict(x[cut:])
    mae = float(np.mean(np.abs(pred - y[cut:]))) if cut < len(y) else 0.0
    dumped = model.dump_model()
    trees = [node_from(t["tree_structure"]) for t in dumped["tree_info"]]
    init = 0.0
    json.dump(
        {"features": names, "init": init, "trees": trees, "holdout_mae": mae},
        open(dst, "w"),
    )
    print(f"trees {len(trees)} features {len(names)} holdout_mae {mae:.4f}")


if __name__ == "__main__":
    main()

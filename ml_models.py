"""
ML signal models — lightweight XGBoost-based prediction for trading signals.
Designed for 2GB RAM constraint: uses small feature sets and efficient training.
"""

import json
import os
import pickle
from typing import Optional, Dict, List, Tuple

import numpy as np
import pandas as pd

# Lazy imports for memory efficiency
_xgb = None
_sklearn = None


def _get_xgb():
    global _xgb
    if _xgb is None:
        import xgboost as xgb
        _xgb = xgb
    return _xgb


def _get_sklearn():
    global _sklearn
    if _sklearn is None:
        from sklearn import model_selection, metrics, preprocessing
        _sklearn = {"model_selection": model_selection, "metrics": metrics,
                     "preprocessing": preprocessing}
    return _sklearn


MODELS_DIR = os.path.join(os.path.dirname(__file__), "models")
os.makedirs(MODELS_DIR, exist_ok=True)


def compute_features(df: pd.DataFrame) -> pd.DataFrame:
    """
    Compute ML features from OHLCV data.
    Kept lightweight for 2GB RAM.
    """
    feat = pd.DataFrame(index=df.index)

    close = df["close"]
    high = df["high"]
    low = df["low"]
    volume = df["volume"]

    # Price features
    for period in [5, 10, 20, 50]:
        feat[f"return_{period}"] = close.pct_change(period)
        feat[f"sma_ratio_{period}"] = close / close.rolling(period).mean()
        feat[f"volatility_{period}"] = close.pct_change().rolling(period).std()

    # Volume features
    feat["volume_sma_ratio"] = volume / volume.rolling(20).mean()
    feat["volume_change"] = volume.pct_change(5)

    # RSI
    delta = close.diff()
    gain = delta.clip(lower=0).ewm(alpha=1/14, min_periods=14, adjust=False).mean()
    loss = (-delta).clip(lower=0).ewm(alpha=1/14, min_periods=14, adjust=False).mean()
    rs = gain / loss
    feat["rsi_14"] = 100 - (100 / (1 + rs))

    # MACD
    ema12 = close.ewm(span=12, adjust=False).mean()
    ema26 = close.ewm(span=26, adjust=False).mean()
    feat["macd"] = ema12 - ema26
    feat["macd_signal"] = feat["macd"].ewm(span=9, adjust=False).mean()
    feat["macd_hist"] = feat["macd"] - feat["macd_signal"]

    # Bollinger Band width & position
    bb_mid = close.rolling(20).mean()
    bb_std = close.rolling(20).std()
    feat["bb_width"] = (2 * bb_std) / bb_mid
    feat["bb_position"] = (close - (bb_mid - 2*bb_std)) / (4*bb_std)

    # ATR (Average True Range)
    tr = pd.concat([
        high - low,
        (high - close.shift()).abs(),
        (low - close.shift()).abs()
    ], axis=1).max(axis=1)
    feat["atr_14"] = tr.rolling(14).mean() / close

    # Day of week (cyclical encoding)
    if hasattr(df.index, 'dayofweek'):
        feat["day_sin"] = np.sin(2 * np.pi * df.index.dayofweek / 7)
        feat["day_cos"] = np.cos(2 * np.pi * df.index.dayofweek / 7)

    return feat


def compute_target(df: pd.DataFrame, horizon: int = 5, threshold: float = 0.02) -> pd.Series:
    """
    Compute classification target: 1 = price up > threshold, 0 = otherwise.

    Args:
        df: OHLCV DataFrame
        horizon: Forward-looking period
        threshold: Minimum return to count as "up"
    """
    future_return = df["close"].shift(-horizon) / df["close"] - 1
    target = (future_return > threshold).astype(int)
    return target


class MLSignalModel:
    """
    XGBoost-based signal model for trading.
    Lightweight: ~50MB RAM usage.
    """

    def __init__(self, symbol: str = "BTC/USDT", horizon: int = 5,
                 threshold: float = 0.02):
        self.symbol = symbol
        self.horizon = horizon
        self.threshold = threshold
        self.model = None
        self.feature_names = None
        self.train_metrics = {}

    def train(self, df: pd.DataFrame, test_size: float = 0.2,
              verbose: bool = True) -> dict:
        """
        Train the model on OHLCV data.

        Args:
            df: OHLCV DataFrame
            test_size: Fraction for test split (uses time-series split, not random)
            verbose: Print progress

        Returns:
            Dict with training metrics
        """
        xgb = _get_xgb()
        sklearn = _get_sklearn()

        features = compute_features(df)
        target = compute_target(df, self.horizon, self.threshold)

        # Align and drop NaN
        combined = pd.concat([features, target.rename("target")], axis=1).dropna()
        X = combined.drop("target", axis=1)
        y = combined["target"]

        self.feature_names = list(X.columns)

        # Time-series split (no look-ahead)
        split_idx = int(len(X) * (1 - test_size))
        X_train, X_test = X.iloc[:split_idx], X.iloc[split_idx:]
        y_train, y_test = y.iloc[:split_idx], y.iloc[split_idx:]

        if verbose:
            print(f"Training ML model for {self.symbol}")
            print(f"  Features: {len(self.feature_names)}")
            print(f"  Train: {len(X_train)} samples | Test: {len(X_test)} samples")
            print(f"  Target distribution: {y_train.mean():.2%} positive")

        # Train XGBoost (lightweight params for 2GB RAM)
        self.model = xgb.XGBClassifier(
            n_estimators=100,
            max_depth=4,
            learning_rate=0.1,
            subsample=0.8,
            colsample_bytree=0.8,
            min_child_weight=5,
            use_label_encoder=False,
            eval_metric="logloss",
            n_jobs=1,  # Memory efficient
            tree_method="hist",  # Memory efficient
        )

        self.model.fit(X_train, y_train, eval_set=[(X_test, y_test)],
                       verbose=False)

        # Evaluate
        y_pred = self.model.predict(X_test)
        y_prob = self.model.predict_proba(X_test)[:, 1]

        metrics_mod = sklearn["metrics"]
        accuracy = metrics_mod.accuracy_score(y_test, y_pred)
        precision = metrics_mod.precision_score(y_test, y_pred, zero_division=0)
        recall = metrics_mod.recall_score(y_test, y_pred, zero_division=0)
        f1 = metrics_mod.f1_score(y_test, y_pred, zero_division=0)

        try:
            auc = metrics_mod.roc_auc_score(y_test, y_prob)
        except ValueError:
            auc = 0.5

        self.train_metrics = {
            "accuracy": round(accuracy, 4),
            "precision": round(precision, 4),
            "recall": round(recall, 4),
            "f1": round(f1, 4),
            "auc": round(auc, 4),
            "train_samples": len(X_train),
            "test_samples": len(X_test),
            "positive_rate": round(y_train.mean(), 4),
        }

        if verbose:
            print(f"  Accuracy:  {accuracy:.4f}")
            print(f"  Precision: {precision:.4f}")
            print(f"  Recall:    {recall:.4f}")
            print(f"  F1:        {f1:.4f}")
            print(f"  AUC:       {auc:.4f}")

            # Feature importance (top 10)
            importance = dict(zip(self.feature_names, self.model.feature_importances_))
            top_features = sorted(importance.items(), key=lambda x: x[1], reverse=True)[:10]
            print(f"\n  Top features:")
            for fname, imp in top_features:
                print(f"    {fname:<25} {imp:.4f}")

        return self.train_metrics

    def predict(self, df: pd.DataFrame) -> pd.DataFrame:
        """
        Generate predictions on new data.
        Returns DataFrame with 'ml_signal' and 'ml_probability' columns.
        """
        if self.model is None:
            raise ValueError("Model not trained. Call train() first.")

        features = compute_features(df)
        features = features[self.feature_names].dropna()

        if features.empty:
            result = df.copy()
            result["ml_signal"] = 0
            result["ml_probability"] = 0.5
            result["signal"] = 0
            return result

        predictions = self.model.predict(features)
        probabilities = self.model.predict_proba(features)[:, 1]

        result = df.copy()
        result["ml_signal"] = 0
        result["ml_probability"] = 0.5

        result.loc[features.index, "ml_signal"] = predictions
        result.loc[features.index, "ml_probability"] = probabilities

        # Convert to trading signals
        result["signal"] = 0
        # Buy when model predicts up with high confidence
        result.loc[result["ml_probability"] > 0.6, "signal"] = 1
        # Sell when model predicts down with high confidence
        result.loc[result["ml_probability"] < 0.4, "signal"] = -1

        return result

    def save(self, path: Optional[str] = None):
        """Save model to disk."""
        if self.model is None:
            raise ValueError("No model to save")
        path = path or os.path.join(MODELS_DIR, f"ml_{self.symbol.replace('/', '_')}.pkl")
        with open(path, "wb") as f:
            pickle.dump({
                "model": self.model,
                "feature_names": self.feature_names,
                "symbol": self.symbol,
                "horizon": self.horizon,
                "threshold": self.threshold,
                "train_metrics": self.train_metrics,
            }, f)
        print(f"Model saved to {path}")

    def load(self, path: Optional[str] = None):
        """Load model from disk."""
        path = path or os.path.join(MODELS_DIR, f"ml_{self.symbol.replace('/', '_')}.pkl")
        with open(path, "rb") as f:
            data = pickle.load(f)
        self.model = data["model"]
        self.feature_names = data["feature_names"]
        self.symbol = data["symbol"]
        self.horizon = data["horizon"]
        self.threshold = data["threshold"]
        self.train_metrics = data.get("train_metrics", {})
        print(f"Model loaded from {path}")


def train_and_backtest_ml(df: pd.DataFrame, symbol: str = "BTC/USDT",
                           horizon: int = 5, threshold: float = 0.02) -> dict:
    """
    Train ML model and backtest it. Convenience function.
    """
    from backtester import Backtester, format_results

    model = MLSignalModel(symbol=symbol, horizon=horizon, threshold=threshold)
    metrics = model.train(df, verbose=True)

    # Generate signals on test portion
    split_idx = int(len(df) * 0.8)
    test_df = df.iloc[split_idx:]
    signals_df = model.predict(test_df)

    # Backtest
    bt = Backtester(initial_capital=1000)
    results = bt.run(signals_df, strategy_name=f"ML-XGBoost-{symbol}",
                    symbol=symbol, timeframe="1d",
                    params={"horizon": horizon, "threshold": threshold},
                    save=True)

    print(format_results(results))
    return {"model_metrics": metrics, "backtest_results": results, "model": model}


if __name__ == "__main__":
    # Test with synthetic data
    np.random.seed(42)
    n = 500
    dates = pd.date_range("2020-01-01", periods=n, freq="D")
    trend = np.cumsum(np.random.randn(n) * 0.02)
    prices = 100 * np.exp(trend)

    df = pd.DataFrame({
        "open": prices * (1 + np.random.randn(n) * 0.005),
        "high": prices * (1 + abs(np.random.randn(n) * 0.01)),
        "low": prices * (1 - abs(np.random.randn(n) * 0.01)),
        "close": prices,
        "volume": np.random.randint(1000, 10000, n).astype(float),
    }, index=dates)

    print("Testing ML model with synthetic data...")
    result = train_and_backtest_ml(df, symbol="TEST/USDT")

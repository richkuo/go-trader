"""
Alert system — prints alerts to stdout (will be wired to Discord later).
Supports different alert levels and formatting.
"""

import json
from datetime import datetime
from typing import Optional, List


class AlertManager:
    """
    Alert manager that routes alerts to stdout.
    Can be extended to send to Discord, email, etc.
    """

    LEVELS = {
        "info": "ℹ️",
        "trade": "💰",
        "warning": "⚠️",
        "error": "❌",
        "critical": "🚨",
    }

    def __init__(self, quiet: bool = False):
        self.quiet = quiet
        self.history: List[dict] = []

    def send(self, title: str, message: str = "", level: str = "info"):
        """
        Send an alert.

        Args:
            title: Alert title
            message: Alert body
            level: info, trade, warning, error, critical
        """
        now = datetime.utcnow()
        icon = self.LEVELS.get(level, "📢")
        alert = {
            "timestamp": now.isoformat(),
            "level": level,
            "title": title,
            "message": message,
        }
        self.history.append(alert)

        if not self.quiet:
            print(f"\n[{now.strftime('%H:%M:%S')}] {icon} {title}")
            if message:
                for line in message.split("\n"):
                    print(f"  {line}")

    def trade_alert(self, symbol: str, side: str, quantity: float, price: float,
                     pnl: Optional[float] = None):
        """Shortcut for trade alerts."""
        icon = "🟢" if side.lower() == "buy" else "🔴"
        msg = f"{icon} {side.upper()} {symbol}: {quantity:.6f} @ ${price:,.2f}"
        if pnl is not None:
            msg += f" | PnL: ${pnl:+,.2f}"
        self.send(f"Trade: {symbol}", msg, level="trade")

    def risk_alert(self, reason: str):
        """Shortcut for risk alerts."""
        self.send("Risk Alert", reason, level="warning")

    def circuit_breaker_alert(self, reason: str):
        """Alert when circuit breaker triggers."""
        self.send("🚨 CIRCUIT BREAKER", reason, level="critical")

    def daily_report_alert(self, report: str):
        """Send daily report."""
        self.send("📊 Daily Report", report, level="info")

    def get_recent(self, n: int = 10) -> List[dict]:
        """Get last N alerts."""
        return self.history[-n:]

    def format_history(self, n: int = 20) -> str:
        """Format recent alert history."""
        alerts = self.get_recent(n)
        if not alerts:
            return "No alerts."
        lines = [f"{'─'*60}", f"  RECENT ALERTS (last {len(alerts)})", f"{'─'*60}"]
        for a in alerts:
            icon = self.LEVELS.get(a["level"], "📢")
            ts = a["timestamp"][:19]
            lines.append(f"  [{ts}] {icon} {a['title']}")
            if a["message"]:
                lines.append(f"    {a['message'][:100]}")
        return "\n".join(lines)


if __name__ == "__main__":
    am = AlertManager()
    am.send("Bot Started", "Paper mode, BTC/USDT", "info")
    am.trade_alert("BTC/USDT", "buy", 0.01, 45000)
    am.trade_alert("BTC/USDT", "sell", 0.01, 46000, pnl=10)
    am.risk_alert("Position size exceeds 20% limit")
    am.circuit_breaker_alert("5 consecutive losses — trading paused")
    print(am.format_history())

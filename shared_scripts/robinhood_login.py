import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "platforms", "robinhood"))


def main():
    if len(sys.argv) > 1:
        sys.stderr.write("robinhood_login.py takes no arguments; run it through: go-trader robinhood-login --config <config.json>\n")
        return 2
    if not sys.stdin.isatty():
        sys.stderr.write("robinhood-login needs an interactive terminal (docker compose run gives one; with systemd-run use --pty)\n")
        return 2
    username = os.environ.get("ROBINHOOD_USERNAME", "")
    password = os.environ.get("ROBINHOOD_PASSWORD", "")
    totp_secret = os.environ.get("ROBINHOOD_TOTP_SECRET", "")
    if not (username.strip() and password.strip() and totp_secret.strip()):
        sys.stderr.write("ROBINHOOD_USERNAME, ROBINHOOD_PASSWORD and ROBINHOOD_TOTP_SECRET must be set (for example from /opt/go-trader/.env)\n")
        return 1

    from adapter import SESSION_DIR_ENV, RobinhoodLoginError, robinhood_account_key, robinhood_login

    try:
        result = robinhood_login(username, password, totp_secret, interactive=True)
    except RobinhoodLoginError:
        return 1
    except KeyboardInterrupt:
        sys.stderr.write("robinhood-login interrupted; the stored session is unchanged\n")
        return 130
    folder = os.environ.get(SESSION_DIR_ENV, "")
    name = f"robinhood-{robinhood_account_key(username)}.pickle"
    mode = os.stat(os.path.join(folder, name)).st_mode & 0o777
    sys.stderr.write(f"Robinhood session {result}: {os.path.join(folder, name)} (mode {mode:04o})\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())

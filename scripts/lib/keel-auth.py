#!/usr/bin/env python3
"""Authenticate an E2E script against a fresh `keel serve`.

The product moved from one deployment passphrase to account login plus CSRF
(change keel-bot-identity-access, alpha zero compatibility). Shell E2E scripts
still need a cookie jar and an `X-Keel-CSRF` value for raw `curl` calls; this
helper produces both without ever writing a password or a setup token to
stdout:

  * `bootstrap` reads a one-time setup token from a 0600 file, sets the first
    super admin's username/password, logs in, and changes the password once
    (the bootstrap path always forces a change).
  * `login` authenticates an existing account.

Output is one JSON line naming the jar and CSRF files. Errors are the server's
own refusal text; no credential is echoed. Passwords arrive through the
environment (`KEEL_ACCOUNT_PASSWORD`, `KEEL_ACCOUNT_NEW_PASSWORD`) so they do
not appear in `ps` or in shell history.

Usage:
  KEEL_ACCOUNT_PASSWORD=… KEEL_ACCOUNT_NEW_PASSWORD=… \
    python3 scripts/lib/keel-auth.py bootstrap \
      --base http://127.0.0.1:13682 \
      --token-file /tmp/setup.token \
      --username e2e-admin \
      --jar /tmp/cookies.txt \
      --csrf-file /tmp/csrf.txt

  KEEL_ACCOUNT_PASSWORD=… \
    python3 scripts/lib/keel-auth.py login \
      --base http://127.0.0.1:13682 \
      --username e2e-admin \
      --jar /tmp/cookies.txt \
      --csrf-file /tmp/csrf.txt
"""

from __future__ import annotations

import argparse
import http.cookiejar
import json
import os
import stat
import sys
import urllib.error
import urllib.request

CSRF_HEADER = "X-Keel-CSRF"


class Failure(RuntimeError):
    pass


class Client:
    def __init__(self, base: str):
        self.base = base.rstrip("/")
        self.jar = http.cookiejar.MozillaCookieJar()
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(self.jar)
        )
        self.csrf = ""
        self.account_id = ""

    def _allow_secure_loopback(self) -> None:
        """Send Secure cookies over plain-HTTP loopback in tests.

        The server marks `keel_session` and the CSRF cookie Secure (correct for
        production). Python's cookie policy refuses to send them over http://
        even though browsers treat loopback as a trustworthy origin, so the
        test helper drops the flag for 127.0.0.1/localhost only. The saved
        Netscape jar therefore stays usable by curl as well.
        """

        if not self.base.startswith("http://"):
            return
        for cookie in self.jar:
            if cookie.domain in ("127.0.0.1", "localhost", "::1"):
                cookie.secure = False

    def _request(self, method: str, path: str, body: dict | None = None) -> dict:
        self._allow_secure_loopback()
        raw = None
        headers = {}
        if body is not None:
            raw = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        if method not in ("GET", "HEAD", "OPTIONS") and self.csrf:
            headers[CSRF_HEADER] = self.csrf
        request = urllib.request.Request(
            self.base + path, data=raw, headers=headers, method=method
        )
        try:
            with self.opener.open(request, timeout=20) as response:
                payload = response.read()
        except urllib.error.HTTPError as err:
            payload = err.read()
            detail = payload.decode("utf-8", "replace")
            raise Failure(
                f"{method} {path}: HTTP {err.code}: {detail[:400]}"
            ) from None
        except urllib.error.URLError as err:
            raise Failure(f"{method} {path}: {err.reason}") from None
        if not payload:
            return {}
        try:
            return json.loads(payload)
        except json.JSONDecodeError:
            raise Failure(f"{method} {path}: response is not JSON") from None

    def session(self) -> dict:
        body = self._request("GET", "/v1/auth/session")
        token = body.get("csrfToken") or ""
        if not token:
            raise Failure("GET /v1/auth/session returned no csrfToken")
        self.csrf = token
        return body

    def _keep_csrf(self, body: dict) -> dict:
        token = body.get("csrfToken") or ""
        if token:
            self.csrf = token
        account = body.get("account") or {}
        if isinstance(account, dict) and account.get("id"):
            self.account_id = str(account["id"])
        return body

    def bootstrap(self, token: str, username: str, password: str) -> dict:
        return self._keep_csrf(
            self._request(
                "POST",
                "/v1/auth/bootstrap",
                {"token": token, "username": username, "password": password},
            )
        )

    def login(self, username: str, password: str) -> dict:
        return self._keep_csrf(
            self._request(
                "POST",
                "/v1/auth/login",
                {"username": username, "password": password},
            )
        )

    def change_password(self, current: str, new: str) -> dict:
        return self._keep_csrf(
            self._request(
                "POST",
                "/v1/auth/password",
                {"currentPassword": current, "newPassword": new},
            )
        )

    def save(self, jar_path: str, csrf_path: str) -> None:
        self.jar.save(jar_path, ignore_discard=True, ignore_expires=True)
        os.chmod(jar_path, stat.S_IRUSR | stat.S_IWUSR)
        with open(csrf_path, "w", encoding="utf-8") as handle:
            handle.write(self.csrf + "\n")
        os.chmod(csrf_path, stat.S_IRUSR | stat.S_IWUSR)


def read_token(path: str) -> str:
    with open(path, encoding="utf-8") as handle:
        token = handle.read().strip()
    if not token:
        raise Failure(f"setup token file {path} is empty")
    return token


def password_from_env(name: str) -> str:
    value = os.environ.get(name, "")
    if not value:
        raise Failure(f"{name} is not set")
    return value


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("bootstrap", "login"))
    parser.add_argument("--base", required=True)
    parser.add_argument("--username", required=True)
    parser.add_argument("--jar", required=True)
    parser.add_argument("--csrf-file", required=True)
    parser.add_argument("--token-file")
    args = parser.parse_args(argv)

    client = Client(args.base)
    try:
        if args.command == "bootstrap":
            if not args.token_file:
                raise Failure("bootstrap needs --token-file")
            token = read_token(args.token_file)
            initial = password_from_env("KEEL_ACCOUNT_PASSWORD")
            final = password_from_env("KEEL_ACCOUNT_NEW_PASSWORD")
            if initial == final:
                raise Failure(
                    "KEEL_ACCOUNT_NEW_PASSWORD must differ from KEEL_ACCOUNT_PASSWORD"
                )
            client.session()
            client.bootstrap(token, args.username, initial)
            # Bootstrap does not establish a session. Log in with the initial
            # password, then satisfy the forced change; the final password is
            # the one the browser/CLI lane uses.
            client.session()
            client.login(args.username, initial)
            client.change_password(initial, final)
        else:
            client.session()
            client.login(
                args.username, password_from_env("KEEL_ACCOUNT_PASSWORD")
            )
    except Failure as err:
        print(f"keel-auth: {err}", file=sys.stderr)
        return 1

    try:
        client.save(args.jar, args.csrf_file)
    except OSError as err:
        print(f"keel-auth: write cookie/CSRF files: {err}", file=sys.stderr)
        return 1
    print(
        json.dumps(
            {
                "authenticated": True,
                "username": args.username,
                "accountId": client.account_id,
                "jar": args.jar,
                "csrfFile": args.csrf_file,
            }
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))

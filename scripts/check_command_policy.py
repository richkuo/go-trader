#!/usr/bin/env python3
import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CHECKER = os.path.join(ROOT, ".claude", "hooks", "command_policy.py")
SETTINGS = os.path.join(ROOT, ".claude", "settings.json")
GO_TRADER_URL = "https://github.com/richkuo/go-trader.git"
ROOT_COMMIT = "444bc81ba17de7824c5a628d0a211f83cc599442"


class Runner(object):
    def __init__(self, tmp, action_scripts):
        self.tmp = tmp
        self.action_scripts = action_scripts
        self.failures = []
        self.passed = 0
        self.skipped = []
        self.home = os.path.join(tmp, "home")
        os.makedirs(self.home)
        self.env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
        self.env.update({"HOME": self.home, "GIT_CONFIG_NOSYSTEM": "1", "GIT_AUTHOR_NAME": "fixture",
                         "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "fixture",
                         "GIT_COMMITTER_EMAIL": "fixture@example.invalid", "LC_ALL": "C"})
        self.env.pop("XDG_CONFIG_HOME", None)
        self.env.pop("CDPATH", None)

    def sh(self, cwd, *args, **kw):
        p = subprocess.run(list(args), cwd=cwd, env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                           universal_newlines=True, timeout=60)
        if p.returncode != 0 and not kw.get("ok_fail"):
            raise RuntimeError("fixture command failed: %s\n%s" % (" ".join(args), p.stderr))
        return p.stdout.strip()

    def repo(self, name, remote=None, branch="main"):
        path = os.path.join(self.tmp, name)
        os.makedirs(os.path.join(path, "scripts"))
        self.sh(path, "git", "init", "-q", "-b", branch)
        with open(os.path.join(path, "README"), "w") as fh:
            fh.write(name + "\n")
        shutil.copy(os.path.join(ROOT, "scripts", "update.sh"), os.path.join(path, "scripts", "update.sh"))
        self.sh(path, "git", "add", "-A")
        self.sh(path, "git", "commit", "-q", "-m", "base")
        if remote:
            self.sh(path, "git", "remote", "add", "origin", remote)
        return path

    def hook(self, payload_obj, raw=None):
        data = raw if raw is not None else json.dumps(payload_obj)
        p = subprocess.run([sys.executable, "-I", CHECKER], input=data, env=self.env, stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, universal_newlines=True, timeout=60)
        return p.returncode, p.stderr.strip()

    def decide(self, cwd, command):
        return self.hook({"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": {"command": command},
                          "cwd": cwd})

    def ref(self, repo, name):
        return self.sh(repo, "git", "rev-parse", "-q", "--verify", name, ok_fail=True)

    def case(self, name, cwd, command, want, execute=False, watch=None):
        rc, err = self.decide(cwd, command)
        got = "allow" if rc == 0 else ("deny" if rc == 2 else "error rc=%d" % rc)
        if got != want:
            self.failures.append("%s: want %s, got %s\n  command: %s\n  stderr: %s" % (name, want, got, command, err))
            return
        if execute and got == "allow" and watch:
            before = self.ref(watch, "refs/heads/main")
            subprocess.run(["bash", "-c", command], cwd=cwd, env=self.env, stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, timeout=60)
            after = self.ref(watch, "refs/heads/main")
            if before != after:
                self.failures.append("%s: the checker allowed the command, and it moved main of %s"
                                     % (name, watch))
                return
        self.passed += 1
        print("ok: %s -> %s" % (name, got))

    def run(self):
        gt = self.repo("gt", GO_TRADER_URL)
        wt = os.path.join(self.tmp, "gt-wt")
        self.sh(gt, "git", "worktree", "add", "-q", "-b", "cc/feature", wt)
        detached = os.path.join(self.tmp, "gt-detached")
        self.sh(gt, "git", "worktree", "add", "-q", "--detach", detached)
        other = self.repo("other")
        shallow = os.path.join(self.tmp, "shallow")
        self.sh(self.tmp, "git", "clone", "-q", "--depth", "1", "file://" + other, shallow)
        self.sh(shallow, "git", "remote", "set-url", "origin", "https://example.invalid/other.git")
        rewritten = self.repo("rewritten", "gt:repo")
        self.sh(rewritten, "git", "config", "url." + GO_TRADER_URL + ".insteadOf", "gt:repo")
        plain = os.path.join(self.tmp, "plain")
        os.makedirs(os.path.join(plain, "scripts"))
        shutil.copy(os.path.join(other, "scripts", "update.sh"), os.path.join(plain, "scripts", "update.sh"))
        self.sh(gt, "git", "config", "alias.cm", "commit")
        self.sh(gt, "git", "config", "alias.st", "status")
        self.sh(gt, "git", "config", "alias.c2", "cm --allow-empty")
        self.sh(gt, "git", "config", "alias.wip", "!git cm --allow-empty -m wip")
        self.sh(gt, "git", "config", "alias.co", "checkout")
        self.sh(gt, "git", "config", "alias.loop", "loop2")
        self.sh(gt, "git", "config", "alias.loop2", "loop")

        c = self.case
        c("commit on go-trader main", gt, "git commit --allow-empty -m probe", "deny")
        c("commit on a feature worktree", wt, "git commit --allow-empty -m probe", "allow", True, gt)
        c("commit on a detached go-trader HEAD", detached, "git commit --allow-empty -m probe", "deny")
        c("commit on an unrelated repository main", other, "git commit --allow-empty -m probe", "allow")
        c("commit on main of a shallow clone that cannot be verified", shallow,
          "git commit --allow-empty -m probe", "deny")
        c("commit on main of a repository whose remote rewrites to go-trader", rewritten,
          "git commit --allow-empty -m probe", "deny")
        c("commit outside any repository", plain, "git commit --allow-empty -m probe", "deny")
        c("cd into the feature worktree then commit", gt, "cd %s && git add -A && git commit --allow-empty -m probe"
          % wt, "allow", True, gt)
        c("cd into main then commit", wt, "cd %s && git commit --allow-empty -m probe" % gt, "deny")
        c("relative cd into main then commit", wt, "cd ../gt && git commit --allow-empty -m probe", "deny")
        c("cd with a semicolon into an existing worktree", gt, "cd %s; git commit --allow-empty -m probe" % wt,
          "allow", True, gt)
        c("failed cd with a semicolon falls back to main", gt, "cd %s/missing; git commit --allow-empty -m probe" % wt,
          "deny")
        c("cd to a missing directory then commit", wt, "cd %s/missing && git commit --allow-empty -m probe" % gt,
          "deny")
        c("cd with a variable then commit", wt, "cd \"$HOME\"/x && git commit -m probe", "deny")
        c("subshell cd does not leak", gt, "(cd %s && true) && git commit --allow-empty -m probe" % wt, "deny")
        c("subshell cd into the worktree", gt, "(cd %s && git commit --allow-empty -m probe)" % wt, "allow", True, gt)
        c("git -C into the worktree", gt, "git -C %s commit --allow-empty -m probe" % wt, "allow", True, gt)
        c("git -C into main", wt, "git -C %s commit --allow-empty -m probe" % gt, "deny")
        c("repeated -C resolves in order", self.tmp, "git -C gt -C ../gt-wt commit --allow-empty -m probe", "allow",
          True, gt)
        c("repeated -C ending on main", self.tmp, "git -C gt-wt -C ../gt commit --allow-empty -m probe", "deny")
        c("--git-dir and --work-tree to main", wt,
          "git --git-dir=%s/.git --work-tree=%s commit --allow-empty -m probe" % (gt, gt), "deny")
        c("--git-dir of the worktree", gt, "git --git-dir %s/.git/worktrees/gt-wt --work-tree %s commit "
          "--allow-empty -m probe" % (gt, wt), "allow", True, gt)
        c("GIT_DIR environment override", wt, "GIT_DIR=%s/.git git commit --allow-empty -m probe" % gt, "deny")
        c("core.worktree configuration override", wt, "git -c core.worktree=%s commit --allow-empty -m probe" % gt,
          "deny")
        c("harmless -c option", wt, "git -c user.name=x commit --allow-empty -m probe", "allow", True, gt)
        c("alias from the command line", gt, "git -c alias.ci=commit ci --allow-empty -m probe", "deny")
        c("alias from repository config on main", gt, "git cm --allow-empty -m probe", "deny")
        c("alias from repository config on a feature branch", wt, "git cm --allow-empty -m probe", "allow", True, gt)
        c("harmless alias", gt, "git st", "allow")
        c("alias chain to a commit alias on main", gt, "git c2 -m x", "deny")
        c("shell alias that runs a commit alias on main", gt, "git wip", "deny")
        c("alias chain to a commit alias on a feature branch", wt, "git c2 -m x", "allow", True, gt)
        c("shell alias that runs a commit alias on a feature branch", wt, "git wip", "allow", True, gt)
        c("checkout alias to main then commit", wt, "git co main && git commit --allow-empty -m x", "deny")
        c("alias loop on main", gt, "git loop", "deny")
        c("unknown git command on main", gt, "git frobnicate", "deny")
        c("unknown git command on a feature branch", wt, "git frobnicate", "allow")
        c("dynamic subcommand on main", gt, "git $SUB -m probe", "deny")
        c("new branch then commit in one chain", gt,
          "git checkout -q -b cc/chain && git commit --allow-empty -m probe && git checkout -q main", "allow",
          True, gt)
        c("switch to main then commit", wt, "git switch main && git commit -m probe", "deny")
        previous = self.repo("previous-branch", GO_TRADER_URL, branch="cc/feature")
        self.sh(previous, "git", "branch", "main")
        self.sh(previous, "git", "switch", "main")
        self.sh(previous, "git", "switch", "cc/feature")
        c("switch to previous main then commit", previous, "git switch @{-1} && git commit --allow-empty -m x",
          "deny")
        c("checkout of main with trailing separator then commit", previous,
          "git checkout main -- && git commit --allow-empty -m x", "deny")
        c("checkout of a path from main then commit", previous,
          "git checkout main -- README && git commit --allow-empty -m x", "allow", True, previous)
        tracking = self.repo("tracking-branch", GO_TRADER_URL, branch="cc/feature")
        self.sh(tracking, "git", "update-ref", "refs/remotes/origin/main", "HEAD")
        c("switch with an implicit tracking branch then commit", tracking,
          "git switch --track origin/main && git commit --allow-empty -m x", "deny")
        c("checkout of a branch that cannot be resolved then commit", wt,
          "git checkout something && git commit -m probe", "deny")
        c("path checkout then commit", wt, "git checkout -- README && git commit --allow-empty -m probe", "allow",
          True, gt)
        c("branch switch in a separate earlier call is not reused", wt, "git commit --allow-empty -m probe", "allow",
          True, gt)
        c("gh pr checkout then commit", wt, "gh pr checkout 12 && git commit -m probe", "deny")
        c("commit in the background next to a branch switch", wt,
          "git commit -m probe & git switch main", "deny")
        c("branch switch in the background next to a commit", wt,
          "git switch main & git commit -m probe", "deny")
        c("export of a git variable then commit", wt, "export GIT_DIR=%s/.git && git commit -m probe" % gt, "deny")
        c("export of an unrelated variable then commit", wt, "export FOO=1 && git commit --allow-empty -m probe",
          "allow", True, gt)
        c("source then commit", wt, "source ./env.sh && git commit -m probe", "deny")
        c("commit message from a here-document", gt,
          "git commit -m \"$(cat <<'EOF'\nmessage (with parens)\nEOF\n)\"", "deny")
        c("commit message from a here-document on a feature branch", wt,
          "git commit --allow-empty -m \"$(cat <<'EOF'\nmessage (with parens)\nEOF\n)\"", "allow", True, gt)
        c("command substitution that switches branch", wt,
          "git commit -m \"$(git checkout main; echo x)\"", "deny")
        c("command name from a substitution", gt, "$(which git) commit -m probe", "deny")
        c("escaped command words", gt, "g\\it com\\mit -m probe", "deny")
        c("absolute git path", gt, "/usr/bin/env git --no-pager commit -m probe", "deny")
        c("comment that mentions a commit", gt, "ls # git commit", "allow")
        c("quoted command text stays data", gt, "echo 'git commit -m x' && printf '%s\\n' \"git commit\"", "allow")
        c("pull request body that mentions commands", gt,
          "gh pr create --title t --body 'run git commit then bash scripts/update.sh'", "allow")
        c("grep for the word commit", gt, "git log --oneline | grep commit", "allow")
        c("bash -c string on main", gt, "bash -c 'git commit -m probe'", "deny")
        c("bash -lc string with cd into the worktree", gt,
          "bash -lc 'cd %s && git commit --allow-empty -m probe'" % wt, "allow", True, gt)
        c("sh here-document that commits on main", gt, "sh <<'EOF'\ngit commit -m probe\nEOF", "deny")
        c("sudo git commit on main", gt, "sudo -n git commit -m probe", "deny")
        c("env -C into main", wt, "env -C %s git commit -m probe" % gt, "deny")
        c("unsupported wrapper around a commit", wt, "xargs git commit -m probe </dev/null", "deny")
        c("dynamic command name with a commit on main", gt, "G=git; $G commit --allow-empty -m x", "deny")
        main_before = self.ref(gt, "refs/heads/main")
        c("unsupported wrapper around a commit alias", gt, "xargs git cm --allow-empty -m x </dev/null", "deny")
        if self.ref(gt, "refs/heads/main") != main_before:
            self.failures.append("the unsupported wrapper check moved main")
        c("function definition with a commit alias", gt, "function f { git cm --allow-empty -m x; }; f", "deny")
        c("eval with a commit alias", gt, "eval 'git cm --allow-empty -m x'", "deny")
        c("parameter expansion with a commit alias", gt, "echo ${x:-$(git cm --allow-empty -m x)}", "deny")
        c("arithmetic expansion with a commit alias", gt, "echo $(( $(git cm --allow-empty -m x) ))", "deny")
        c("unsupported wrapper around a known git subcommand", gt, "find . -name '*.go' | xargs git add", "allow")
        main_before = self.ref(gt, "refs/heads/main")
        c("branch switch through xargs then commit", wt,
          "echo main | xargs git checkout && git commit --allow-empty -m x", "deny")
        if self.ref(gt, "refs/heads/main") != main_before:
            self.failures.append("the xargs branch switch check moved main")
        c("branch switch in a function then commit", wt,
          "function f { git switch main; }; f; git commit --allow-empty -m x", "deny")
        c("branch switch in a parameter expansion then commit", wt,
          "echo ${x:-$(git checkout main)} && git commit --allow-empty -m x", "deny")
        c("commit before a branch switch through xargs", wt,
          "git commit --allow-empty -m x; echo main | xargs git checkout", "deny")
        c("branch switch in a bash -c string then commit", wt,
          "bash -c 'git switch main' && git commit --allow-empty -m x", "deny")
        c("new branch in a bash -c string then commit", gt,
          "bash -c 'git switch -q -c cc/nested' && git commit --allow-empty -m x && git switch -q main", "allow",
          True, gt)
        c("whole command in a variable on main", gt, 'C="git commit --allow-empty -m x"; $C', "deny")
        c("commit alias in a variable through bash -c", gt, 'C="git cm --allow-empty -m x"; bash -c "$C"', "deny")
        c("commit alias in a variable through eval", gt, 'C="git cm --allow-empty -m x"; eval "$C"', "deny")
        c("commit text piped into a shell", gt, "echo 'git commit --allow-empty -m x' | bash", "deny")
        c("variable command name without git", gt, "PY=python3; $PY -c 'print(1)'", "allow")
        c("whole command in a variable through xargs", gt, 'C="git commit --allow-empty -m x"; xargs $C </dev/null',
          "deny")
        c("whole command in a variable through find -exec", gt,
          'C="git commit --allow-empty -m x"; find . -maxdepth 0 -exec $C \\;', "deny")
        c("ssh with a variable host and a variable remote command", gt,
          'H=localhost; C="git commit --allow-empty -m x"; ssh "$H" "$C"', "deny")
        c("variable argument to an ordinary command next to git", gt,
          "T=x; python3 -I -c 'print(1)' \"$T\" && git status", "allow")
        c("variable find word that can hold -exec next to git", gt,
          "D=.; find \"$D\" -maxdepth 0 -name x && git status", "deny")
        c("variable -exec operator for find with a variable command", gt,
          'O=-exec; C="git commit --allow-empty -m x"; find . -maxdepth 0 "$O" $C \\;', "deny")
        c("literal find without -exec next to git", gt, "find . -maxdepth 0 -name x && git status", "allow")
        c("commit words from standard input into xargs git on main", gt,
          "echo commit --allow-empty -m x | xargs git", "deny")
        c("updater path from standard input into xargs bash", gt, "echo scripts/update.sh | xargs bash", "deny")
        c("commit text from standard input into xargs sh -c", gt,
          "printf '%s\\0' 'git commit --allow-empty -m x' | xargs -0 sh -c", "deny")
        c("checkout words from standard input into xargs git then commit", wt,
          "echo checkout main | xargs git && git commit --allow-empty -m x", "deny")
        c("unrelated command that names git", gt, "brew install git", "allow")
        c("awk program that runs a commit", gt, "awk 'BEGIN{system(\"git commit --allow-empty -m x\")}'", "deny")
        c("sed program that runs a commit", gt, "sed '1e git commit --allow-empty -m x' README", "deny")
        c("awk program that runs a command from a variable", gt,
          "C='git commit --allow-empty -m x'; awk -v c=\"$C\" 'BEGIN{system(c)}'", "deny")
        c("awk text processing next to git", gt, "awk '{print $1}' README && git status", "allow")
        c("sed text processing after git", gt, "git log -1 | sed -n 1p", "allow")
        c("xargs replace string that turns a known subcommand into commit", gt,
          "echo commit | xargs -I add git add --allow-empty -m x", "deny")
        c("xargs replace string after another option", gt,
          "echo commit | xargs -n 1 -I status git status --allow-empty -m x", "deny")
        c("parallel replacement into a git subcommand", gt, "parallel git {} --allow-empty -m x ::: commit", "deny")
        c("sed e command that runs commit text from standard input", gt,
          "echo 'git commit --allow-empty -m x' | sed e", "deny")
        c("sed s command with the e flag next to git", gt, "sed 's/^/git /e' README", "deny")
        c("awk system call on input lines next to git", gt,
          "echo 'git commit --allow-empty -m x' | awk '{system($0)}'", "deny")
        c("sed substitution with an e in its text next to git", gt, "sed 's/old/new/' README && git status", "allow")
        c("sed script from a file next to git", gt, "sed -f x.sed README && git status", "deny")
        c("xargs replace option inside a grouped flag", gt, "echo commit | xargs -0I add git add --allow-empty -m x",
          "deny")
        c("xargs replace option after a bare -l", gt, "echo commit | xargs -l -Iadd git add --allow-empty -m x",
          "deny")
        c("xargs abbreviated replace option", gt, "echo commit | xargs --rep=add git add --allow-empty -m x", "deny")
        c("xargs with known options before git add", gt, "find . -name '*.go' -print0 | xargs -0 -n 20 git add",
          "allow")
        c("sed e flag inside a grouped -ne option next to git", gt, "sed -ne 's/^/git /e' README", "deny")
        c("sed abbreviated --expression option next to git", gt, "sed --expr=e README && git status", "deny")
        c("sed unknown option next to git", gt, "sed -x 1p README && git status", "deny")
        c("sed in-place edit next to git", gt, "sed -i.bak 's/a/b/' README && git status", "allow")
        c("awk abbreviated --source option next to git", gt, "awk --so 'BEGIN{}' && git status", "deny")
        c("awk indirect call next to git", gt, "awk '{@f()}' README && git status", "deny")
        c("awk with a field separator next to git", gt, "awk -F: '{print $1}' README && git status", "allow")
        c("conditional with a known git subcommand", gt, "if git diff --quiet; then echo same; fi", "allow")
        c("conditional with quoted git text", gt, "if true; then echo 'git cm'; fi", "allow")
        c("nested shell with a dynamic git subcommand", gt, 'bash -c "git $SUB --allow-empty -m x"', "deny")
        c("unsupported wrapper with git options before a commit alias", gt,
          "xargs git -C . cm --allow-empty -m x </dev/null", "deny")
        c("unsupported wrapper with git options before a known subcommand", gt,
          "find . -name '*.go' | xargs git -C . add", "allow")
        c("parallel with no command runs commit lines from standard input", gt,
          "echo 'git commit --allow-empty -m x' | parallel", "deny")
        c("xargs replace string in the command name next to git", gt,
          "echo git | xargs -I{} {} commit --allow-empty -m x", "deny")
        c("find -exec runs the found file as a commit", gt,
          "find /usr/bin /usr/local/bin /opt/homebrew/bin -name git -exec {} commit --allow-empty -m x \\;",
          "deny")
        c("find -exec with a replaced command name next to git", gt,
          "find . -name x -exec {} --version \\; && git status", "deny")
        c("xargs runs a wrapper that runs a git branch switch", gt, "echo x | xargs env git checkout main", "deny")
        c("xargs runs find with a shell next to git", gt,
          "echo x | xargs find . -exec sh -c 'git status' \\;", "deny")
        c("xargs runs a sed program that runs commands next to git", gt,
          "git ls-files | xargs sed '1e git commit'", "deny")
        c("xargs search for a runner name in files that git lists", gt,
          "git ls-files '*.go' | xargs grep -n timeout", "allow")
        c("xargs search for the word git in files that git lists", gt, "git ls-files | xargs grep -l git", "allow")
        c("xargs search with a variable pattern next to git", gt,
          "echo x | xargs -0 -n 2 grep -l \"$P\" && git status", "allow")
        c("xargs sed search for commit text with -- before the file names", gt,
          "git ls-files | xargs sed -n '/git commit/p' --", "allow")
        c("find -exec search for a runner name next to git", gt,
          "find . -name '*.go' -exec grep -n timeout {} + && git status", "allow")
        c("sed search for commit text", gt, "sed -n '/git commit/p' SKILL.md", "allow")
        c("sed edit of a git command in a doc", gt, "sed -i.bak 's/git status/git status --short/' README", "allow")
        c("awk search for push text", gt, "awk '/git push/' README", "allow")
        c("sed program with a variable next to git", gt, "sed \"s/x/$Y/\" README && git status", "deny")
        c("xargs -i replace string into a git subcommand", gt,
          "echo commit | xargs -i git {} --allow-empty -m x", "deny")
        c("xargs --replace without a value into a git subcommand", gt,
          "echo commit | xargs --replace git {} --allow-empty -m x", "deny")
        c("xargs replace string into a sed flag next to git", gt,
          "echo e | xargs -I@ sed 's/^/git status/@' README", "deny")
        c("xargs replace string into an awk function name next to git", gt,
          "echo system | xargs -I Q awk 'BEGIN{Q(\"git commit --allow-empty -m x\")}'", "deny")
        c("find -exec replace string into a sed flag next to git", gt,
          "find . -name e -exec sed 's/^/git status/{}' README \\;", "deny")
        c("nested xargs keeps the outer replace string for sed next to git", gt,
          "echo e | xargs -I@ xargs sed 's/^/git status/@' README", "deny")
        c("xargs sed search with a replace string in the file name next to git", gt,
          "git ls-files | xargs -I{} sed -n 1p {}", "deny")
        c("xargs appends an -e option with commit text to sed", gt,
          "echo \"-e '1e git commit --allow-empty -m x'\" | xargs sed -n p README", "deny")
        c("xargs appends an -exec option with a commit to find", gt,
          "echo '-exec git commit --allow-empty -m x ;' | xargs find . -maxdepth 0", "deny")
        c("xargs appends words to an awk program next to git", gt,
          "git ls-files | xargs awk '/git push/'", "deny")
        c("xargs sed search for commit text without -- before the file names", gt,
          "git ls-files | xargs sed -n '/git commit/p'", "deny")
        c("xargs sed with -- taken as the -l value", gt,
          "echo \"-e '1e git commit --allow-empty -m x'\" | xargs sed -n p -l --", "deny")
        c("xargs sed with -- taken as the --line-length value", gt,
          "echo \"-e '1e git commit --allow-empty -m x'\" | xargs sed -n p --line-length --", "deny")
        c("xargs sed with -- before the script", gt, "echo \"1e git commit --allow-empty -m x\" | xargs sed -n --",
          "deny")
        c("if statement around a commit", wt, "if true; then git commit -m probe; fi", "deny")
        c("unparseable command with a commit", wt, "git commit -m 'unterminated", "deny")
        c("unparseable case with a commit alias", gt, "case 1 in 1) git cm --allow-empty -m x;; esac", "deny")
        c("unparseable nested function with a commit alias", gt,
          "bash -c 'f() { :; }; git cm --allow-empty -m x'", "deny")
        c("unparseable command without a guarded call", wt, "echo 'unterminated", "allow")
        c("arithmetic expansion that hides a commit", wt, "echo $(( $(git commit -m x) ))", "deny")
        c("ssh remote commit cannot be checked", wt, "ssh host 'cd /opt/go-trader && git commit -m x'", "deny")
        c("ordinary build commands", gt, "go -C scheduler build . && uv run --no-sync python -c 'print(1)'", "allow")

        c("update without restart", gt, "bash scripts/update.sh", "deny")
        c("syntax check of updater", gt, "bash -n scripts/update.sh", "allow")
        c("syntax check of updater with grouped flags", gt, "bash -en -- scripts/update.sh", "allow")
        c("lint check of updater", gt, "shellcheck scripts/update.sh", "allow")
        c("updater with syntax check disabled", gt, "bash -n +n scripts/update.sh", "deny")
        c("update with the restart flag", gt, "bash scripts/update.sh --restart", "allow")
        c("update with the restart flag and an absolute path", wt, "bash %s/scripts/update.sh --restart" % gt, "allow")
        c("update by direct execution without restart", gt, "./scripts/update.sh", "deny")
        c("update by direct execution with restart", gt, "./scripts/update.sh --restart --unit go-trader-live",
          "allow")
        c("update with RESTART=1", gt, "RESTART=1 bash scripts/update.sh", "allow")
        c("update with RESTART=0", gt, "RESTART=0 bash scripts/update.sh", "deny")
        c("update with RESTART=1 then RESTART=0", gt, "RESTART=1 RESTART=0 bash scripts/update.sh", "deny")
        c("update with RESTART=1 dropped by sudo", gt, "RESTART=1 sudo bash scripts/update.sh", "deny")
        c("update with RESTART=1 forwarded by sudo", gt, "sudo RESTART=1 bash scripts/update.sh", "allow")
        c("update with RESTART=1 forwarded by env", gt, "env RESTART=1 bash scripts/update.sh", "allow")
        c("update with RESTART=1 removed by env -u", gt, "RESTART=1 env -u RESTART bash scripts/update.sh", "deny")
        c("update with RESTART=1 removed by env -i", gt, "RESTART=1 env -i bash scripts/update.sh", "deny")
        c("update with RESTART exported earlier", gt, "export RESTART=1 && bash scripts/update.sh", "deny")
        c("update where --restart is the operand of --update-all-root", gt,
          "bash scripts/update.sh --update-all-root --restart", "deny")
        c("update where --restart is the operand of --rsync-from", gt,
          "bash scripts/update.sh --rsync-from --restart", "deny")
        c("update with a dynamic argument", gt, "bash scripts/update.sh \"$FLAGS\"", "deny")
        c("update help", gt, "bash scripts/update.sh --help", "allow")
        c("update help after a valid option", gt, "bash scripts/update.sh --unit x -h", "allow")
        c("update help after an invalid option", gt, "bash scripts/update.sh --bogus --help", "deny")
        c("update recovery-clear", gt, "sudo bash scripts/update.sh --clear-recovery", "allow")
        c("update recovery-clear with --all", gt, "bash scripts/update.sh --clear-recovery --all", "deny")
        c("update --all with restart", gt, "bash scripts/update.sh --all --restart", "allow")
        c("update from the worktree checkout", wt, "bash scripts/update.sh", "deny")
        c("update after cd into go-trader", self.tmp, "cd gt && bash scripts/update.sh", "deny")
        c("go-trader updater run from an unrelated checkout", other, "bash %s/scripts/update.sh" % gt, "deny")
        c("unrelated updater in an unrelated repository", other, "bash scripts/update.sh", "allow")
        c("unrelated updater outside any repository", plain, "bash scripts/update.sh", "allow")
        c("updater found on PATH", gt, "update.sh", "deny")
        c("updater through ssh without restart", wt, "ssh host 'cd /opt/go-trader && bash scripts/update.sh'", "deny")
        c("updater through ssh with restart", wt, "ssh host 'cd /opt/go-trader && sudo bash scripts/update.sh "
          "--restart'", "allow")
        c("updater in a bash -c string", gt, "bash -c 'bash scripts/update.sh'", "deny")
        c("updater through nohup with restart", gt, "nohup bash scripts/update.sh --restart >/tmp/u.log 2>&1 &",
          "allow")
        c("updater sourced without restart", gt, "source scripts/update.sh", "deny")
        c("development build stays outside the update rule", gt, "go -C scheduler build -o go-trader .", "allow")
        c("image update stays outside the update rule", gt, "docker compose pull && docker compose up -d", "allow")
        c("scheduler update sequence is not a Bash call", gt, "go -C scheduler test -run TestUpdate ./...", "allow")

        self.malformed()
        self.wrapper(gt, wt)
        self.root_identity()

    def malformed(self):
        checks = [
            ("invalid JSON input", None, "{not json", 2),
            ("non-object JSON input", None, "[]", 2),
            ("missing command text", {"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": {}}, None,
             2),
            ("missing tool name", {"hook_event_name": "PreToolUse", "tool_input": {"command": "ls"}}, None, 2),
            ("another tool passes", {"hook_event_name": "PreToolUse", "tool_name": "Read",
                                     "tool_input": {"file_path": "/x"}}, None, 0),
            ("another event passes", {"hook_event_name": "PostToolUse", "tool_name": "Bash",
                                      "tool_input": {"command": "git commit -m x"}}, None, 0),
            ("relative cwd cannot resolve a commit", {"hook_event_name": "PreToolUse", "tool_name": "Bash",
                                                      "tool_input": {"command": "git commit -m x"}, "cwd": "rel"},
             None, 2),
        ]
        for name, obj, raw, want in checks:
            rc, err = self.hook(obj, raw)
            if rc != want:
                self.failures.append("%s: want rc %d, got %d (%s)" % (name, want, rc, err))
            else:
                self.passed += 1
                print("ok: %s -> rc %d" % (name, rc))

    def wrapper(self, gt, wt):
        synthetic = os.path.join(self.tmp, "synthetic-action", "scripts")
        os.makedirs(synthetic)
        for name in ("git-commit.sh", "git-env.sh"):
            with open(os.path.join(synthetic, name), "w") as fh:
                fh.write("#!/usr/bin/env bash\nexec git commit \"$@\"\n")
        with open(os.path.join(synthetic, ".git-repo"), "w") as fh:
            fh.write("%s/.git/worktrees/gt-wt\n%s\n%s\n" % (gt, wt, self.home))
        self.case("synthetic wrapper with valid-looking metadata", wt,
                  "%s/git-commit.sh -m probe" % synthetic, "deny")
        if not self.action_scripts:
            self.skipped.append("the pinned action wrapper cases (set --action-scripts to the action's scripts/)")
            return
        action = os.path.join(self.tmp, "action", "scripts")
        os.makedirs(action)
        for name in ("git-commit.sh", "git-env.sh"):
            shutil.copy(os.path.join(self.action_scripts, name), os.path.join(action, name))
        os.chmod(os.path.join(action, "git-commit.sh"), 0o755)
        wrapper = os.path.join(action, "git-commit.sh")
        meta = os.path.join(action, ".git-repo")
        main_record = "%s\n%s\n%s\n" % (os.path.realpath(os.path.join(gt, ".git")), os.path.realpath(gt), self.home)
        wt_gitdir = self.sh(wt, "git", "rev-parse", "--absolute-git-dir")
        wt_record = "%s\n%s\n%s\n" % (wt_gitdir, os.path.realpath(wt), self.home)

        def record(text):
            if os.path.exists(meta):
                os.chmod(meta, 0o644)
                os.remove(meta)
            if text is not None:
                with open(meta, "w") as fh:
                    fh.write(text)

        record(main_record)
        self.case("action wrapper targets main while the shell is in the feature worktree", wt,
                  "%s --allow-empty -m probe" % wrapper, "deny")
        self.ground_truth("action wrapper targets main", wt, wrapper, gt, True)
        record(wt_record)
        self.case("action wrapper targets the feature worktree while the shell is on main", gt,
                  "%s --allow-empty -m probe" % wrapper, "allow", True, gt)
        self.case("action wrapper through bash", gt, "bash %s --allow-empty -m probe" % wrapper, "allow", True, gt)
        self.case("action wrapper after cd", self.tmp, "cd gt && %s --allow-empty -m probe" % wrapper, "allow", True,
                  gt)
        self.ground_truth("action wrapper targets the feature worktree", gt, wrapper, gt, False)
        self.case("branch switch in the wrapper target before the wrapper", gt,
                  "git -C %s switch main; %s --allow-empty -m probe" % (wt, wrapper), "deny")
        record(None)
        self.case("action wrapper without a repository record", wt, "%s --allow-empty -m probe" % wrapper, "deny")
        record("%s\n%s\n" % (os.path.join(self.tmp, "missing", ".git"), wt))
        self.case("action wrapper with an inconsistent repository record", wt, "%s --allow-empty -m probe" % wrapper,
                  "deny")
        record("relative/.git\n%s\n" % wt)
        self.case("action wrapper with a relative repository record", wt, "%s --allow-empty -m probe" % wrapper,
                  "deny")
        record(wt_record)

    def ground_truth(self, name, cwd, wrapper, gt, expect_main_moves):
        before = self.ref(gt, "refs/heads/main")
        p = subprocess.run([wrapper, "--allow-empty", "-m", "ground truth"], cwd=cwd, env=self.env,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True, timeout=60)
        after = self.ref(gt, "refs/heads/main")
        moved = before != after
        if p.returncode != 0 or moved != expect_main_moves:
            self.failures.append("%s: the real wrapper run did not match the checker target (rc %d, main moved %s)\n%s"
                                 % (name, p.returncode, moved, p.stderr))
            return
        if moved:
            self.sh(gt, "git", "update-ref", "refs/heads/main", before)
        self.passed += 1
        print("ok: %s -> the real wrapper commit landed %s" % (name, "on main" if moved else "off main"))

    def root_identity(self):
        p = subprocess.run(["git", "-C", ROOT, "cat-file", "commit", ROOT_COMMIT], stdout=subprocess.PIPE,
                           stderr=subprocess.PIPE, env=self.env)
        if p.returncode != 0:
            self.skipped.append("the root-commit identity case (this checkout does not hold the go-trader root "
                                "commit, as in a shallow clone)")
            return
        repo = self.repo("rooted", "https://example.invalid/fork.git")
        w = subprocess.run(["git", "hash-object", "-t", "commit", "-w", "--literally", "--stdin"], cwd=repo,
                           input=p.stdout, stdout=subprocess.PIPE, env=self.env)
        if w.stdout.decode().strip() != ROOT_COMMIT:
            self.failures.append("root identity fixture: the copied root commit has a different id")
            return
        self.case("commit on main of a repository that holds the go-trader root commit", repo,
                  "git commit --allow-empty -m probe", "deny")
        self.case("updater in a repository that holds the go-trader root commit", repo, "bash scripts/update.sh",
                  "deny")


def check_settings():
    with open(SETTINGS) as fh:
        data = json.load(fh)
    hooks = data["hooks"]["PreToolUse"]
    if len(hooks) != 1 or hooks[0].get("matcher") != "Bash":
        raise SystemExit("FAIL: .claude/settings.json must hold one PreToolUse Bash matcher")
    entry = hooks[0]["hooks"]
    if len(entry) != 1:
        raise SystemExit("FAIL: .claude/settings.json must hold one command hook")
    h = entry[0]
    if h.get("type") != "command" or h.get("onFailure") != "block" or h.get("async"):
        raise SystemExit("FAIL: the hook must be a synchronous command hook with onFailure block")
    if not isinstance(h.get("timeout"), int) or not 0 < h["timeout"] <= 60:
        raise SystemExit("FAIL: the hook needs a finite timeout of at most 60 seconds")
    if ".claude/hooks/command_policy.py" not in h.get("command", "") or " -I " not in h.get("command", ""):
        raise SystemExit("FAIL: the hook must run .claude/hooks/command_policy.py with python3 -I")
    print("ok: .claude/settings.json holds one synchronous Bash command hook with onFailure block")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--action-scripts", default=os.environ.get("COMMAND_POLICY_ACTION_SCRIPTS", ""))
    ap.add_argument("--require-action-scripts", action="store_true")
    a = ap.parse_args()
    if a.require_action_scripts and not a.action_scripts:
        raise SystemExit("FAIL: --require-action-scripts needs --action-scripts or COMMAND_POLICY_ACTION_SCRIPTS")
    check_settings()
    tmp = tempfile.mkdtemp(prefix="command-policy-")
    try:
        r = Runner(os.path.realpath(tmp), a.action_scripts)
        r.run()
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    for s in r.skipped:
        print("SKIP: " + s)
    if r.failures:
        for f in r.failures:
            print("FAIL: " + f)
        return 1
    if a.require_action_scripts and any("action wrapper" in s for s in r.skipped):
        print("FAIL: the pinned action wrapper cases did not run")
        return 1
    print("PASS: command policy (%d checks)" % r.passed)
    return 0


if __name__ == "__main__":
    sys.exit(main())

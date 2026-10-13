# Security Policy

## Reporting a vulnerability

Please report security issues privately through GitHub's
[private vulnerability reporting](https://github.com/KnobanLabs/linux-ephemeral-runners/security/advisories/new)
(the **Report a vulnerability** button on the Security tab). Don't open a public
issue or pull request for a vulnerability.

Include what you found, how to reproduce it, and what an attacker could do with
it. We'll acknowledge the report, keep you updated while we work on a fix, and
credit you in the advisory unless you'd rather not be named.

## Supported versions

Only the latest commit on `develop` is supported. There are no released
versions yet.

## Scope

In scope: the controller, the install scripts, and anything that lets a job
reach beyond its own VM, outlive its job, read another job's data, or obtain a
deployment's GitHub credentials.

Out of scope: vulnerabilities in QEMU, KVM, the Linux kernel, the
GitHub Actions runner, or the VM images; please report those to their own
projects. Running these runners for public repositories is unsupported; see
"Security model" in the [README](README.md#security-model).

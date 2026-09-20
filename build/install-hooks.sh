#!/usr/bin/env bash
#
# TinyVMOS — installe les hooks git locaux.
#
# GitHub refuse les rulesets sur un depot prive sans abonnement Pro. Ce hook
# rend la meme garantie cote poste de travail : aucun push direct sur main,
# tout passe par une branche et une pull request.
#
# Usage : ./build/install-hooks.sh

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
HOOK="$REPO_ROOT/.git/hooks/pre-push"

cat > "$HOOK" <<'HOOKEOF'
#!/usr/bin/env bash
# Refuse tout push direct vers main. Voir CONTRIBUTING.md.
set -euo pipefail

PROTECTED="main"

while read -r _local_ref _local_sha remote_ref _remote_sha; do
    branch=${remote_ref#refs/heads/}
    if [ "$branch" = "$PROTECTED" ]; then
        cat >&2 <<'MSG'

  Push direct sur main refuse.

  main ne se met a jour que par fusion d une pull request.

    git switch -c feat/mon-changement
    git push -u origin feat/mon-changement
    gh pr create --fill

  Voir CONTRIBUTING.md. Pour passer outre en connaissance de cause :
  git push --no-verify

MSG
        exit 1
    fi
done

exit 0
HOOKEOF

chmod 0755 "$HOOK"
echo "Hook pre-push installe : $HOOK"
echo "main est desormais protegee localement contre les pushes directs."

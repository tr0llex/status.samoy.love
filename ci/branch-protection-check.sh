#!/usr/bin/env bash
# Сверка имён задач текущего workflow со списком обязательных проверок main.
#
#   ci/branch-protection-check.sh
#
# ЗАЧЕМ ЭТОТ ФАЙЛ. Слияние или переименование задач CI не ломает сам прогон —
# он остаётся зелёным. Ломается защита ветки: она хранит СТАРЫЕ имена, ждёт
# их вечно, и PR не мержится без единого красного шага, объясняющего почему.
# Поломка при этом отложенная: её видно не в момент переименования, а на
# следующем PR, когда «Merge» неактивен без объяснения. Правится руками
# (Settings → Branches), но руками же и забывается.
#
# Источник фактических имён — не разбор YAML, а Jobs API того же прогона:
# GitHub сам разворачивает матрицы и вызовы переиспользуемых workflow в
# имена check runs, и сверяться нужно ровно с тем, что видит защита ветки, а
# не с тем, что написано в файле. Имена задач в contexts — это `name:` задачи
# (или её ID, если имя не задано), это и есть контракт с branch protection.
#
# Направление расхождения — любое: пропавшая обязательная проверка навсегда
# блокирует PR (см. выше), а новая незащищённая проверка — тихая дыра: её
# красный цвет никого не остановит.
#
# Сама эта задача — исключение по построению: она проверяет остальных, а не
# саму себя, и не может требовать своего появления в защите заранее (первый
# прогон после добавления задачи защиту ещё не видел). Имя передаётся через
# SELF_JOB_NAME и вычитается только из стороны «лишних», не из вывода.

set -uo pipefail

: "${GITHUB_REPOSITORY:?нужен GITHUB_REPOSITORY (запуск вне GitHub Actions?)}"
: "${GITHUB_RUN_ID:?нужен GITHUB_RUN_ID (запуск вне GitHub Actions?)}"
: "${GH_TOKEN:?нужен PAT с правом Administration: Read-only (secrets.PROTECTION_READ_TOKEN) — у GITHUB_TOKEN такого scope не бывает}"

BASE_BRANCH="${BASE_BRANCH:-main}"

command -v gh >/dev/null || { echo "нужен gh CLI" >&2; exit 1; }
command -v jq >/dev/null || { echo "нужен jq" >&2; exit 1; }

# Один снимок Jobs API не гарантирует полноты. GitHub регистрирует задачу с
# needs только когда та реально стартовала, и даже уже удовлетворённые needs
# (частичный перезапуск --failed по уже готовым зависимостям) оставляют
# секундный зазор, пока список догоняет реальность. Опрашиваем, пока два
# снимка подряд не совпадут — задачи в рамках одного прогона только
# появляются, никогда не пропадают, так что растущий список рано или поздно
# стабилизируется, а стабильный дважды подряд — и есть полный.
echo "текущие задачи прогона ($GITHUB_RUN_ID):"
prev=""
current=""
for _ in 1 2 3 4 5; do
    current="$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/jobs" --paginate \
        -q '.jobs[].name' 2>/tmp/current.err)" || {
        echo "::error::не удалось получить список задач прогона" >&2
        cat /tmp/current.err >&2
        exit 1
    }
    current="$(sort -u <<<"$current")"
    [[ "$current" == "$prev" ]] && break
    prev="$current"
    sleep 3
done
sed 's/^/  /' <<<"$current"

echo
echo "обязательные проверки ветки $BASE_BRANCH:"
required="$(gh api "repos/${GITHUB_REPOSITORY}/branches/${BASE_BRANCH}/protection/required_status_checks" \
    -q '.contexts[]' 2>/tmp/required.err)"
rc=$?
if [[ $rc -ne 0 ]]; then
    if grep -qi 'HTTP 404' /tmp/required.err; then
        echo "у ветки $BASE_BRANCH нет обязательных проверок — сверять не с чем"
        exit 0
    fi
    echo "::error::не удалось получить защиту ветки $BASE_BRANCH" >&2
    cat /tmp/required.err >&2
    echo "::error::если это отказ в доступе — правам GITHUB_TOKEN может не хватать 'administration: read' (см. permissions в ci.yml)" >&2
    exit 1
fi
required="$(sort -u <<<"$required")"
sed 's/^/  /' <<<"$required"

missing="$(comm -23 <(printf '%s\n' "$required") <(printf '%s\n' "$current"))"
extra="$(comm -13 <(printf '%s\n' "$required") <(printf '%s\n' "$current"))"
if [[ -n "${SELF_JOB_NAME:-}" ]]; then
    extra="$(grep -vFx "$SELF_JOB_NAME" <<<"$extra" || true)"
fi

fail=0

if [[ -n "$missing" ]]; then
    echo
    echo "::error::обязательные проверки ветки $BASE_BRANCH, которых нет среди текущих задач (переименованы или удалены):"
    while IFS= read -r name; do
        echo "::error::  - $name"
    done <<<"$missing"
    fail=1
fi

if [[ -n "$extra" ]]; then
    echo
    echo "::error::задачи прогона, не входящие в обязательные проверки $BASE_BRANCH (новые или переименованные, но не защищённые):"
    while IFS= read -r name; do
        echo "::error::  - $name"
    done <<<"$extra"
    fail=1
fi

if [[ $fail -eq 0 ]]; then
    echo
    echo "совпадает"
fi

exit $fail

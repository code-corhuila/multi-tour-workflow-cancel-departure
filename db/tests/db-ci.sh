#!/bin/sh
# I·db_ci_builds_reruns_rolls_back_and_rebuilds and I·workflow_cancel_departure_app_writes_only_its_schema.
#
# Builds the workflow_cancel_departure schema from an EMPTY PostgreSQL, applies the migrations a second time (zero
# changes), rolls everything back (empty again) and applies them once more (Annex A, db-ci; Annex J,
# J.6 and J.10). It starts its own throwaway PostgreSQL and Liquibase containers on a private network,
# with passwords generated now, so it touches nothing else and writes no secret. Docker is the only
# requirement; the CI and a developer run this same file.
set -u
cd "$(dirname "$0")/../.." || exit 1

# Git Bash on Windows rewrites container paths such as /workspace; no effect elsewhere
export MSYS_NO_PATHCONV=1

POSTGRES_IMAGE="postgres:18.6-alpine"
RUN_ID="wfl4-dbci-$$"
# the same image the runner of deploy/compose.yml uses: Liquibase plus the PostgreSQL driver
LIQUIBASE_IMAGE="$RUN_ID-liquibase"
DB="multitour_test"
ADMIN_PASSWORD="admin-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
APP_PASSWORD="app-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
fail=0

cleanup() {
  docker rm -f "$RUN_ID" >/dev/null 2>&1 || true
  docker network rm "$RUN_ID" >/dev/null 2>&1 || true
  docker rmi "$LIQUIBASE_IMAGE" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 1' INT TERM

psql_admin() { docker exec -e PGPASSWORD="$ADMIN_PASSWORD" "$RUN_ID" psql -h 127.0.0.1 -U postgres -d "$DB" -v ON_ERROR_STOP=1 -tA "$@"; }

# Each -db owns its control table (J.6); the runner uses the instance administrator (ADR-005 E7)
lb() {
  docker run --rm --network "$RUN_ID" -v "$PWD/db:/workspace:ro" \
    -e LIQUIBASE_COMMAND_URL="jdbc:postgresql://$RUN_ID:5432/$DB" \
    -e LIQUIBASE_COMMAND_USERNAME=postgres -e LIQUIBASE_COMMAND_PASSWORD="$ADMIN_PASSWORD" \
    "$LIQUIBASE_IMAGE" --search-path=/workspace --changelog-file=changelog/changelog-master.yaml \
    --database-changelog-table-name=databasechangelog_workflow_cancel_departure \
    --database-changelog-lock-table-name=databasechangeloglock_workflow_cancel_departure "$@"
}

check() { # name, then the command that must succeed
  name=$1; shift
  if reason=$("$@" 2>&1); then
    echo "PASS  $name"
  else
    echo "FAIL  $name"
    printf '%s\n' "$reason" | tail -n 8 | sed 's/^/        /'
    fail=1
  fi
}

# The schemas where workflow_cancel_departure_app can write, by the privilege query of J.10: only its own may appear
writable_schemas() {
  psql_admin -c "SELECT string_agg(DISTINCT table_schema, ',' ORDER BY table_schema) FROM information_schema.tables
    WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
      AND has_table_privilege('workflow_cancel_departure_app', format('%I.%I', table_schema, table_name), 'INSERT,UPDATE,DELETE')"
}

# A positive control first: the check must SEE a write privilege when one exists, or it would pass
# only because the other schema is out of reach. The grant is revoked right after.
writes_only_its_schema() {
  psql_admin -c "GRANT INSERT ON other_domain.probe TO workflow_cancel_departure_app" >/dev/null || return 1
  seen=$(writable_schemas)
  psql_admin -c "REVOKE INSERT ON other_domain.probe FROM workflow_cancel_departure_app" >/dev/null || return 1
  [ "$seen" = "other_domain,workflow_cancel_departure" ] || { echo "the privilege query did not see a granted write: '$seen'"; return 1; }
  [ "$(writable_schemas)" = "workflow_cancel_departure" ] || { echo "workflow_cancel_departure_app can write in: $(writable_schemas)"; return 1; }
  [ "$(psql_admin -c "SELECT has_schema_privilege('workflow_cancel_departure_app', 'public', 'CREATE')")" = "f" ] || { echo "workflow_cancel_departure_app can create in public"; return 1; }
  [ "$(psql_admin -c "SELECT has_schema_privilege('workflow_cancel_departure_app', 'workflow_cancel_departure', 'CREATE')")" = "f" ] || { echo "workflow_cancel_departure_app can create objects in workflow_cancel_departure"; return 1; }
}

# Its own control table, login-less roles without a password, and the grant of the writer role to the instance user
owns_its_control_table_and_roles() {
  [ "$(psql_admin -c "SELECT count(*) FROM information_schema.tables WHERE table_name IN ('databasechangelog_workflow_cancel_departure', 'databasechangeloglock_workflow_cancel_departure')")" = 2 ] || { echo "the control tables with the workflow suffix are missing"; return 1; }
  [ "$(psql_admin -c "SELECT count(*) FROM information_schema.tables WHERE table_name IN ('databasechangelog', 'databasechangeloglock')")" = 0 ] || { echo "a default-named control table exists, two domains would share it"; return 1; }
  [ "$(psql_admin -c "SELECT count(*) FROM pg_authid WHERE rolname IN ('workflow_cancel_departure_reader', 'workflow_cancel_departure_writer') AND NOT rolcanlogin AND rolpassword IS NULL")" = 2 ] || { echo "the roles must be NOLOGIN and have no password"; return 1; }
  [ "$(psql_admin -c "SELECT pg_has_role('workflow_cancel_departure_app', 'workflow_cancel_departure_writer', 'MEMBER')")" = "t" ] || { echo "workflow_cancel_departure_app is not a member of workflow_cancel_departure_writer"; return 1; }
}

# The state of the control table, so a second update that applied anything shows as a difference
changelog_state() {
  psql_admin -c "SELECT count(*) || ':' || coalesce(md5(string_agg(id || md5sum, ',' ORDER BY orderexecuted)), '') FROM databasechangelog_workflow_cancel_departure"
}

second_update_changes_nothing() {
  before=$(changelog_state)
  out=$(lb update 2>&1) || { printf '%s\n' "$out" | tail -n 8; return 1; }
  [ "$before" = "$(changelog_state)" ] || { echo "the second update changed the control table"; return 1; }
  case $before in 0:*) echo "no changeset was ever recorded"; return 1 ;; esac
}

rolled_back_to_empty() {
  lb rollback-count 999 >/dev/null 2>&1 || { echo "rollback-count 999 failed"; return 1; }
  [ "$(psql_admin -c "SELECT count(*) FROM information_schema.schemata WHERE schema_name = 'workflow_cancel_departure'")" = 0 ] || { echo "the schema workflow_cancel_departure is still there"; return 1; }
  [ "$(psql_admin -c "SELECT count(*) FROM pg_roles WHERE rolname IN ('workflow_cancel_departure_reader', 'workflow_cancel_departure_writer')")" = 0 ] || { echo "a workflow role is still there"; return 1; }
  [ "$(psql_admin -c "SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member WHERE r.rolname = 'workflow_cancel_departure_app'")" = 0 ] || { echo "workflow_cancel_departure_app is still a member of a role"; return 1; }
  [ "$(psql_admin -c "SELECT count(*) FROM pg_default_acl")" = 0 ] || { echo "a default privilege is left behind"; return 1; }
  [ "$(psql_admin -c "SELECT count(*) FROM databasechangelog_workflow_cancel_departure")" = 0 ] || { echo "the control table still lists changesets"; return 1; }
}

# Each changeset is idempotent by itself (Annex A): its SQL, applied again over a built schema, fails nowhere
changesets_apply_twice_without_error() {
  for d in 01_ddl/01_schemas 01_ddl/03_tables 01_ddl/04_alter 01_ddl/10_indexes 03_dcl/00_roles 03_dcl/01_grants; do
    for f in db/$d/*.sql; do
      out=$(docker exec -i -e PGPASSWORD="$ADMIN_PASSWORD" "$RUN_ID" psql -h 127.0.0.1 -U postgres -d "$DB" -v ON_ERROR_STOP=1 -q < "$f" 2>&1) || { echo "applying $f twice failed: $out"; return 1; }
    done
  done
}
# ---- the probes of the saga tables (HU-WFL-004, task WF4-02): what the engine refuses
S1=00000000-0000-4000-8000-0000000000a1
S2=00000000-0000-4000-8000-0000000000a2
GHOST=00000000-0000-4000-8000-0000000000ff
TENANT=00000000-0000-4000-8000-0000000000c1
OTHER_TENANT=00000000-0000-4000-8000-0000000000c2
DEPARTURE=00000000-0000-4000-8000-0000000000d1
RESERVATION=00000000-0000-4000-8000-0000000000e1
RESERVATION2=00000000-0000-4000-8000-0000000000e2
KEY1=key-cancel-0001
KEY2=key-cancel-0002
T=workflow_cancel_departure.cancel_departure_saga

# a statement the database must refuse, for the reason named by a fragment of its message
refused() { # fragment, statement
  out=$(psql_admin -c "$2" 2>&1) && { echo "the database accepted: $2"; return 1; }
  printf '%s' "$out" | grep -q "$1" || { echo "refused, but not for the expected reason ($1): $out"; return 1; }
}
clean() { psql_admin -c "TRUNCATE ${T}_reservation, ${T}_step, $T" >/dev/null || { echo "could not clean the tables"; return 1; }; }
saga() { # id, idempotency key, status, failed step (quoted or NULL), reason (quoted or NULL), tenant, departure code
  printf "INSERT INTO %s (saga_id, idempotency_key, saga_status, failed_step, failure_reason, tenant_id, departure_code) VALUES ('%s', '%s', '%s', %s, %s, '%s', '%s')" \
    "$T" "$1" "$2" "${3:-RUNNING}" "${4:-NULL}" "${5:-NULL}" "${6:-$TENANT}" "${7:-SAL-001}"
}
step() { # saga id, number, name
  printf "INSERT INTO %s_step (saga_id, step_number, step_name) VALUES ('%s', %s, '%s')" "$T" "$1" "$2" "$3"
}
reservation() { # saga id, reservation id, state
  printf "INSERT INTO %s_reservation (saga_id, reservation_id, reservation_state) VALUES ('%s', '%s', '%s')" "$T" "$1" "$2" "${3:-PENDING}"
}

idempotency_key_is_unique_per_tenant_and_sized() {
  clean; psql_admin -c "$(saga $S1 $KEY1)" >/dev/null || return 1
  refused uq_cancel_departure_saga_idempotency_key "$(saga $S2 $KEY1)" || return 1
  refused chk_cancel_departure_saga_idempotency_key "$(saga $S2 short)" || return 1
  psql_admin -c "$(saga $S2 $KEY1 RUNNING NULL NULL $OTHER_TENANT)" >/dev/null
}
status_is_one_of_the_four_states_and_compensated_only_when_the_cancellation_was_refused() {
  clean
  U=00000000-0000-4000-8000-0000000000
  psql_admin -c "$(saga ${U}b1 key-state-running RUNNING)" -c "$(saga ${U}b2 key-state-completed COMPLETED)" \
             -c "$(saga ${U}b3 key-state-compensated COMPENSATED "'CANCEL_DEPARTURE'" "'DEPARTURE_REFUSED'")" \
             -c "$(saga ${U}b4 key-state-failed FAILED "'OPEN_EPISODES'" "'RESERVATIONS_FAILED'")" >/dev/null || return 1
  refused chk_cancel_departure_saga_status "$(saga $S1 $KEY1 DONE)" || return 1
  # nothing is undone in this saga: COMPENSATED means only that the cancellation was refused and nothing changed
  refused chk_cancel_departure_saga_compensated "$(saga $S1 $KEY1 COMPENSATED "'OPEN_EPISODES'" "'RESERVATIONS_FAILED'")"
}
failed_step_goes_with_a_failed_status_a_known_name_and_a_known_reason() {
  clean
  refused 'chk_cancel_departure_saga_failure"' "$(saga $S1 $KEY1 FAILED NULL "'RESERVATIONS_FAILED'")" || return 1
  refused 'chk_cancel_departure_saga_failure"' "$(saga $S1 $KEY1 RUNNING "'OPEN_EPISODES'" "'RESERVATIONS_FAILED'")" || return 1
  refused chk_cancel_departure_saga_failure_pair "$(saga $S1 $KEY1 FAILED "'OPEN_EPISODES'")" || return 1
  refused chk_cancel_departure_saga_failed_step "$(saga $S1 $KEY1 FAILED "'NO_SUCH_STEP'" "'RESERVATIONS_FAILED'")" || return 1
  refused chk_cancel_departure_saga_failure_reason "$(saga $S1 $KEY1 FAILED "'OPEN_EPISODES'" "'BECAUSE'")"
}
the_request_is_bounded() {
  clean
  refused chk_cancel_departure_saga_departure_code "INSERT INTO $T (saga_id, idempotency_key, tenant_id, departure_code) VALUES ('$S1', '$KEY1', '$TENANT', '')" || return 1
  refused chk_cancel_departure_saga_departure_code "$(saga $S1 $KEY1 RUNNING NULL NULL $TENANT "$(printf 'X%.0s' $(seq 1 41))")"
}
the_departure_id_and_the_instant_of_the_cancellation_are_kept_together() {
  clean
  refused chk_cancel_departure_saga_cancellation "INSERT INTO $T (saga_id, idempotency_key, tenant_id, departure_code, departure_id) VALUES ('$S1', '$KEY1', '$TENANT', 'SAL-001', '$DEPARTURE')" || return 1
  refused chk_cancel_departure_saga_cancellation "INSERT INTO $T (saga_id, idempotency_key, tenant_id, departure_code, cancelled_at) VALUES ('$S1', '$KEY1', '$TENANT', 'SAL-001', now())" || return 1
  psql_admin -c "$(saga $S1 $KEY1)" >/dev/null || return 1
  [ "$(psql_admin -c "SELECT departure_id IS NULL AND cancelled_at IS NULL FROM $T WHERE saga_id = '$S1'")" = t ] || { echo "a new saga must start with no departure id and no instant"; return 1; }
  psql_admin -c "INSERT INTO $T (saga_id, idempotency_key, tenant_id, departure_code, departure_id, cancelled_at) VALUES ('$S2', '$KEY2', '$TENANT', 'SAL-002', '$GHOST', now())" >/dev/null
}
a_step_is_recorded_once_with_a_known_name_and_number() {
  clean; psql_admin -c "$(saga $S1 $KEY1)" -c "$(step $S1 1 CANCEL_DEPARTURE)" >/dev/null || return 1
  refused pk_cancel_departure_saga_step "$(step $S1 1 CANCEL_DEPARTURE)" || return 1
  refused chk_cancel_departure_saga_step_number "$(step $S1 4 OPEN_EPISODES)" || return 1
  refused chk_cancel_departure_saga_step_name "$(step $S1 2 NO_SUCH_STEP)" || return 1
  refused chk_cancel_departure_saga_step_order "$(step $S1 1 OPEN_EPISODES)" || return 1
  refused chk_cancel_departure_saga_step_order "$(step $S1 3 CANCEL_DEPARTURE)" || return 1
  psql_admin -c "$(step $S1 2 VOID_PROPOSALS)" -c "$(step $S1 3 OPEN_EPISODES)" >/dev/null
}
a_reservation_is_listed_once_per_saga_with_a_known_state() {
  clean; psql_admin -c "$(saga $S1 $KEY1)" -c "$(saga $S2 $KEY2)" -c "$(reservation $S1 $RESERVATION)" >/dev/null || return 1
  refused pk_cancel_departure_saga_reservation "$(reservation $S1 $RESERVATION)" || return 1
  refused chk_cancel_departure_saga_reservation_state "$(reservation $S1 $RESERVATION2 DONE)" || return 1
  psql_admin -c "$(reservation $S1 $RESERVATION2 OPENED)" -c "$(reservation $S2 $RESERVATION FAILED)" >/dev/null
}
steps_and_reservations_follow_their_saga() {
  clean
  refused fk_cancel_departure_saga_step_saga "$(step $GHOST 1 CANCEL_DEPARTURE)" || return 1
  refused fk_cancel_departure_saga_reservation_saga "$(reservation $GHOST $RESERVATION)" || return 1
  psql_admin -c "$(saga $S1 $KEY1)" -c "$(step $S1 1 CANCEL_DEPARTURE)" -c "$(reservation $S1 $RESERVATION)" >/dev/null || return 1
  psql_admin -c "DELETE FROM $T WHERE saga_id = '$S1'" >/dev/null || return 1
  [ "$(psql_admin -c "SELECT (SELECT count(*) FROM ${T}_step) + (SELECT count(*) FROM ${T}_reservation)")" = 0 ] || { echo "the steps and the reservations did not follow their saga"; return 1; }
}
the_app_user_writes_the_tables_and_the_index_exists() {
  for t in cancel_departure_saga cancel_departure_saga_step cancel_departure_saga_reservation; do
    for p in INSERT UPDATE; do
      [ "$(psql_admin -c "SELECT has_table_privilege('workflow_cancel_departure_app', 'workflow_cancel_departure.$t', '$p')")" = t ] || { echo "workflow_cancel_departure_app cannot $p workflow_cancel_departure.$t"; return 1; }
    done
    # a saga is never removed by the service (the list form of has_table_privilege is true when ANY privilege is held, so one at a time)
    [ "$(psql_admin -c "SELECT has_table_privilege('workflow_cancel_departure_app', 'workflow_cancel_departure.$t', 'DELETE')")" = f ] || { echo "workflow_cancel_departure_app can DELETE workflow_cancel_departure.$t"; return 1; }
  done
  [ "$(psql_admin -c "SELECT count(*) FROM pg_indexes WHERE schemaname = 'workflow_cancel_departure' AND indexname = 'idx_cancel_departure_saga_status_updated_at'")" = 1 ] || { echo "the resumption index is missing"; return 1; }
}

# ---- an empty instance, as multi-tour-infra-postgres leaves it for this domain (J.7)
docker build -q -f deploy/liquibase.Dockerfile -t "$LIQUIBASE_IMAGE" deploy >/dev/null || { echo "the runner image did not build"; exit 1; }
docker network create "$RUN_ID" >/dev/null || exit 1
docker run -d --name "$RUN_ID" --network "$RUN_ID" -e POSTGRES_DB="$DB" -e POSTGRES_PASSWORD="$ADMIN_PASSWORD" "$POSTGRES_IMAGE" >/dev/null || exit 1
ready=0
for _ in $(seq 1 60); do
  if docker exec "$RUN_ID" pg_isready -h 127.0.0.1 -U postgres -d "$DB" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || { echo "the throwaway PostgreSQL did not start"; exit 1; }
psql_admin -c "CREATE ROLE workflow_cancel_departure_app LOGIN PASSWORD '$APP_PASSWORD'" \
           -c "ALTER ROLE workflow_cancel_departure_app SET search_path = workflow_cancel_departure" \
           -c "CREATE SCHEMA other_domain" -c "CREATE TABLE other_domain.probe (id int)" >/dev/null || exit 1

check "I·db_ci_builds_from_empty" lb update
check "I·db_ci_second_update_changes_nothing" second_update_changes_nothing
check "I·changesets_apply_twice_without_error" changesets_apply_twice_without_error
check "I·workflow_cancel_departure_app_writes_only_its_schema" writes_only_its_schema
check "I·owns_its_control_table_and_roles" owns_its_control_table_and_roles
check "I·idempotency_key_is_unique_per_tenant_and_sized" idempotency_key_is_unique_per_tenant_and_sized
check "I·status_is_one_of_the_four_states_and_compensated_only_when_the_cancellation_was_refused" status_is_one_of_the_four_states_and_compensated_only_when_the_cancellation_was_refused
check "I·failed_step_goes_with_a_failed_status_a_known_name_and_a_known_reason" failed_step_goes_with_a_failed_status_a_known_name_and_a_known_reason
check "I·the_request_is_bounded" the_request_is_bounded
check "I·the_departure_id_and_the_instant_of_the_cancellation_are_kept_together" the_departure_id_and_the_instant_of_the_cancellation_are_kept_together
check "I·a_step_is_recorded_once_with_a_known_name_and_number" a_step_is_recorded_once_with_a_known_name_and_number
check "I·a_reservation_is_listed_once_per_saga_with_a_known_state" a_reservation_is_listed_once_per_saga_with_a_known_state
check "I·steps_and_reservations_follow_their_saga" steps_and_reservations_follow_their_saga
check "I·the_app_user_writes_the_tables_and_the_index_exists" the_app_user_writes_the_tables_and_the_index_exists
clean
check "I·db_ci_rolls_back_to_empty" rolled_back_to_empty
check "I·db_ci_rebuilds_after_the_rollback" lb update
check "I·workflow_cancel_departure_app_writes_only_its_schema_after_the_rebuild" writes_only_its_schema
check "I·owns_its_control_table_and_roles_after_the_rebuild" owns_its_control_table_and_roles

# the single name the spec uses for the whole sequence
[ "$fail" = 0 ] && echo "PASS  I·db_ci_builds_reruns_rolls_back_and_rebuilds"
[ "$fail" = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$fail"

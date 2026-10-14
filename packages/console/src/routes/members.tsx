import { isOrganizationAdmin, useMe } from "../lib/me";
import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { errorMessage } from "../lib/error";
import {
  createInvitations,
  invitationResourceID,
  listInvitations,
  listMembers,
  MAX_INVITATIONS_PER_REQUEST,
  memberResourceID,
  parseInviteEmails,
  removeMember,
  revokeInvitation,
  updateMemberRole,
  type InvitationResult,
  type MemberRole,
  type OrganizationInvitation,
  type OrganizationMember,
} from "../lib/members";
import { DataTable, EmptyCell, LoadMore, RowActions, RowError } from "../ui/DataTable";
import { ConfirmDialog, Dialog, FormDialog } from "../ui/Dialog";
import { Field, TextArea } from "../ui/Field";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { Notice } from "../ui/Notice";
import { QueryState } from "../ui/QueryState";
import { Select, type SelectOption } from "../ui/Select";
import { StatusBadge } from "../ui/StatusBadge";
import { createRowActions } from "../ui/rowActions";
import { cx, toneClass, ui } from "../ui/styles";

const OWNER_ROLE_OPTION: SelectOption<MemberRole> = { value: "owner", label: "Owner" };

const NON_OWNER_ROLE_OPTIONS: SelectOption<MemberRole>[] = [
  { value: "admin", label: "Admin" },
  { value: "developer", label: "Developer" },
  { value: "viewer", label: "Viewer" },
];

const OWNER_ROLE_OPTIONS: SelectOption<MemberRole>[] = [OWNER_ROLE_OPTION, ...NON_OWNER_ROLE_OPTIONS];

const EMAIL_FAILED_MESSAGE = "Invitation emails could not be sent. Nobody was invited.";
const NO_EMAIL_DELIVERY = "Email delivery is not set up for this Helmr installation, so members can't be invited.";

const MEMBERS_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage organization members.",
  not_found: "This member or invitation is no longer available.",
  service_unavailable: NO_EMAIL_DELIVERY,
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

const membersErrorMessage = (error: unknown) => errorMessage(error, INTERNAL_ERROR_MESSAGE, MEMBERS_ERROR_MESSAGES);

// Why one address of an invitation request was not invited.
const INVITATION_FAILURE_REASONS: Record<string, string> = {
  invalid_email: "Not a valid email address",
  already_member: "Already a member",
  invitation_pending: "Already has a pending invitation",
  owner_role_required: "Has a pending owner invitation",
  email_failed: "Invitation emails could not be sent",
  email_rejected: "Refused by the email server",
};

function invitationFailureReason(result: InvitationResult): string {
  return INVITATION_FAILURE_REASONS[result.error?.code ?? ""] ?? "Invitation could not be created";
}

function memberName(member: OrganizationMember): string {
  return member.display_name || member.email || member.user_id;
}

function canManageFromMe(role?: string | null, permissions?: string[]): boolean {
  if (isOrganizationAdmin(role)) return true;
  return (permissions ?? []).some((permission) =>
    [
      "members:write",
      "members:manage",
      "invitations:write",
      "invitations:manage",
      "org:write",
      "org:admin",
    ].includes(permission),
  );
}

function InviteMembersDialog(props: {
  currentUserRole: string | null | undefined;
  onClose: () => void;
  onInvited: (results: InvitationResult[]) => Promise<void>;
}) {
  const [text, setText] = createSignal("");
  const [role, setRole] = createSignal<MemberRole>("developer");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const roleOptions = createMemo(() =>
    props.currentUserRole === "owner" ? OWNER_ROLE_OPTIONS : NON_OWNER_ROLE_OPTIONS,
  );
  const entries = createMemo(() => parseInviteEmails(text()));
  const valid = createMemo(() => entries().filter((entry) => entry.state === "valid").map((entry) => entry.email));
  const invalidCount = () => entries().filter((entry) => entry.state === "invalid").length;
  const duplicateCount = () => entries().filter((entry) => entry.state === "duplicate").length;
  const overLimit = () => valid().length > MAX_INVITATIONS_PER_REQUEST;
  const summary = () => [
    `${valid().length} to invite`,
    invalidCount() > 0 ? `${invalidCount()} invalid` : "",
    duplicateCount() > 0 ? `${duplicateCount()} ${duplicateCount() === 1 ? "duplicate" : "duplicates"}` : "",
  ].filter(Boolean).join(" · ");

  const submit = async () => {
    if (valid().length === 0 || overLimit()) return;
    setError(null);
    setSubmitting(true);
    try {
      const results = await createInvitations({ emails: valid(), role: role() });
      // When no email could be sent nothing was created: keep the form and its
      // addresses so the same invitations can be sent again.
      if (results.every((result) => result.error?.code === "email_failed" || result.error?.code === "email_rejected")) {
        const refused = results.find((result) => result.error?.code === "email_rejected");
        setError(refused ? `${EMAIL_FAILED_MESSAGE} The email server refused ${refused.email}.` : EMAIL_FAILED_MESSAGE);
        return;
      }
      await props.onInvited(results);
    } catch (createError) {
      setError(membersErrorMessage(createError));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <FormDialog
      title="Invite members"
      intro="Invitations expire after 7 days."
      submitLabel="Invite"
      busyLabel="Inviting..."
      busy={submitting()}
      disabled={valid().length === 0 || overLimit()}
      error={error()}
      onSubmit={() => void submit()}
      onClose={props.onClose}
    >
      <Field
        label="Emails"
        hint="Separate addresses with commas, spaces or new lines."
        error={overLimit() ? `Invite at most ${MAX_INVITATIONS_PER_REQUEST} people at a time.` : null}
      >
        <TextArea value={text()} rows={4} autocomplete="off" spellcheck={false} onInput={(event) => setText(event.currentTarget.value)} autofocus />
      </Field>
      <Show when={entries().length > 0}>
        <div class="mb-3 grid gap-1.5">
          <p class={cx("m-0", ui.fieldHint)} aria-live="polite">{summary()}</p>
          <ul class="m-0 flex max-h-28 list-none flex-wrap gap-1 overflow-y-auto p-0" aria-label="Recipients">
            <For each={entries()}>
              {(entry) => (
                <li
                  class={cx(
                    "inline-flex items-center gap-1 rounded-xs border px-1.5 py-0.5 font-mono text-badge",
                    entry.state === "valid" ? "border-console-border-strong bg-console-bg-panel text-console-text" : entry.state === "invalid" ? toneClass.danger : toneClass.neutral,
                  )}
                >
                  {entry.email}
                  <Show when={entry.state !== "valid"}>
                    <span class="font-sans">· {entry.state === "invalid" ? "invalid" : "duplicate"}</span>
                  </Show>
                </li>
              )}
            </For>
          </ul>
        </div>
      </Show>
      <Field label="Role">
        <Select<MemberRole> value={role()} options={roleOptions()} onChange={setRole} minWidth="100%" />
      </Field>
    </FormDialog>
  );
}

// The outcome of an invitation request: how many people were invited, then
// each address that was not, with why.
function InvitationResultsDialog(props: { results: InvitationResult[]; onClose: () => void }) {
  const invited = () => props.results.filter((result) => result.status === "invited").length;
  const failed = () => props.results.filter((result) => result.status === "failed");
  return (
    <Dialog title="Invitations" onClose={props.onClose}>
      <div class={ui.blockStack}>
        <Show when={invited() > 0}>
          <p class="m-0 text-body leading-normal text-console-text">Invited {invited()} {invited() === 1 ? "person" : "people"}.</p>
        </Show>
        <Show when={failed().length > 0}>
          <div class="grid gap-1.5">
            <p class="m-0 text-body leading-normal text-console-text">Couldn't invite {failed().length}:</p>
            <ul class="m-0 grid list-none gap-1 p-0" aria-label="Not invited">
              <For each={failed()}>
                {(result) => (
                  <li class="text-body leading-normal text-console-muted">
                    <span class="font-mono text-chrome text-console-text">{result.email}</span> · {invitationFailureReason(result)}
                  </li>
                )}
              </For>
            </ul>
          </div>
        </Show>
      </div>
      <div class={ui.modalActions}>
        <button type="button" class={ui.button} onClick={props.onClose}>Done</button>
      </div>
    </Dialog>
  );
}

// One table for the organization: members, then pending invitations in the
// server's order (the invited email as the member, invited time as joined), with
// the status telling them apart.
const COLUMNS = ["Member", "Email", "Role", "Status", "Joined", { label: "Actions", srOnly: true }] as const;

function MemberRow(props: {
  member: OrganizationMember;
  canManage: boolean;
  isCurrentUser: boolean;
  currentUserRole: string | null | undefined;
  busy: boolean;
  error: string | null;
  onRoleChange: (member: OrganizationMember, role: MemberRole) => void;
  onRemove: (member: OrganizationMember) => void;
}) {
  const id = () => memberResourceID(props.member);
  const isOwner = () => props.member.role === "owner";
  const isActive = () => props.member.status === "active";
  const roleOptions = createMemo<SelectOption<MemberRole>[]>(() => {
    if (props.isCurrentUser) {
      if (props.member.role === "owner") return [OWNER_ROLE_OPTION, NON_OWNER_ROLE_OPTIONS[0]!];
      if (props.member.role === "admin") return [NON_OWNER_ROLE_OPTIONS[0]!];
      return NON_OWNER_ROLE_OPTIONS.filter((option) => option.value === props.member.role);
    }
    if (props.currentUserRole === "owner") return OWNER_ROLE_OPTIONS;
    return NON_OWNER_ROLE_OPTIONS;
  });
  const canChangeRole = () => isActive() && props.canManage && id() !== "" && roleOptions().length > 1 && (!isOwner() || props.currentUserRole === "owner");
  const canRemove = () => isActive() && props.canManage && id() !== "" && !props.isCurrentUser && (!isOwner() || props.currentUserRole === "owner");
  return (
    <>
      <tr>
        <td><strong class="font-medium text-console-text">{memberName(props.member)}</strong></td>
        <td><Show when={props.member.email} fallback={<EmptyCell />}>{props.member.email}</Show></td>
        <td>
          <Show when={canChangeRole()} fallback={<StatusBadge resource="role" status={props.member.role} />}>
            <Select<MemberRole>
              value={props.member.role}
              options={roleOptions()}
              onChange={(nextRole) => props.onRoleChange(props.member, nextRole)}
              disabled={props.busy}
              ariaLabel={`Role for ${memberName(props.member)}`}
              minWidth="132px"
            />
          </Show>
        </td>
        <td><StatusBadge resource="member" status={props.member.status ?? "active"} /></td>
        <td><RelativeTime value={props.member.created_at} /></td>
        <RowActions
          label={`Actions for ${memberName(props.member)}`}
          items={canRemove() ? [{ label: "Remove", disabled: props.busy, tone: "danger", onSelect: () => props.onRemove(props.member) }] : []}
        />
      </tr>
      <RowError columns={COLUMNS.length} message={props.error} />
    </>
  );
}

function InvitationRow(props: {
  invitation: OrganizationInvitation;
  canManage: boolean;
  onRevoke: (invitation: OrganizationInvitation) => void;
}) {
  const id = () => invitationResourceID(props.invitation);
  return (
    <tr>
      <td><strong class="font-medium text-console-text">{props.invitation.email}</strong></td>
      <td>{props.invitation.email}</td>
      <td><StatusBadge resource="role" status={props.invitation.role} /></td>
      <td><StatusBadge resource="member" status="invited" /></td>
      <td><RelativeTime value={props.invitation.created_at} /></td>
      <RowActions
        label={`Actions for ${props.invitation.email}`}
        items={props.canManage && id() !== "" ? [{ label: "Revoke", tone: "danger", onSelect: () => props.onRevoke(props.invitation) }] : []}
      />
    </tr>
  );
}

export function Members() {
  const queryClient = useQueryClient();
  const [inviting, setInviting] = createSignal(false);
  const [inviteResults, setInviteResults] = createSignal<InvitationResult[] | null>(null);
  const [removing, setRemoving] = createSignal<OrganizationMember | null>(null);
  const [revoking, setRevoking] = createSignal<OrganizationInvitation | null>(null);
  const memberRows = createRowActions<"role">();

  const me = useMe();
  const members = createQuery(() => ({
    queryKey: ["members"],
    queryFn: listMembers,
    retry: false,
  }));
  const invitations = createInfiniteQuery(() => ({
    queryKey: ["invitations"],
    queryFn: ({ pageParam }) => listInvitations(pageParam || undefined),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    retry: false,
  }));

  const canManage = createMemo(() => canManageFromMe(me.data?.role, me.data?.permissions));
  const emailDelivery = () => me.data?.email_delivery === true;
  // The server lists pending invitations, newest first; further pages are
  // appended in that order.
  const pendingInvitations = createMemo(() =>
    invitations.data?.pages.flatMap((page) => page.invitations) ?? [],
  );

  const invalidateMembers = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["members"] }),
      queryClient.invalidateQueries({ queryKey: ["invitations"] }),
    ]);
  };

  const changeMemberRole = async (member: OrganizationMember, role: MemberRole) => {
    const id = memberResourceID(member);
    if (!id || role === member.role) return;
    await memberRows.run(id, "role", async () => {
      await updateMemberRole(id, role, member.role);
      await queryClient.invalidateQueries({ queryKey: ["members"] });
    }, membersErrorMessage);
  };

  const isCurrentUser = (member: OrganizationMember) =>
    member.user_id === me.data?.user_id || memberResourceID(member) === me.data?.user_id;

  return (
    <>
      <PageHeader
        title="Members"
        actions={
          <Show when={canManage()}>
            <button class={ui.button} type="button" disabled={!emailDelivery()} onClick={() => setInviting(true)}>
              Invite members
            </button>
          </Show>
        }
      />

      <Show when={!me.isPending && !canManage()}>
        <Notice tone="info" class="mb-3">Member settings require admin access.</Notice>
      </Show>
      <Show when={canManage() && !emailDelivery()}>
        <Notice tone="info" class="mb-3">{NO_EMAIL_DELIVERY}</Notice>
      </Show>

      <QueryState
        query={members}
        pending="Loading members..."
        error={membersErrorMessage}
      >
        {(data) => <>
          <Show when={invitations.isError && !invitations.isFetchNextPageError}>
            <Notice
              tone="danger"
              class="mb-3"
              action={<button type="button" class={ui.compactButton} onClick={() => void invitations.refetch()}>Retry</button>}
            >
              Could not load invitations.
            </Notice>
          </Show>
          <DataTable
            columns={COLUMNS}
            minWidth="min-w-210"
            footer={<LoadMore count={data().members.length} noun={{ one: "member", other: "members" }} extra={{ count: pendingInvitations().length, noun: { one: "invitation", other: "invitations" } }} hasMore={invitations.hasNextPage} loading={invitations.isFetchingNextPage} failed={invitations.isFetchNextPageError} onLoadMore={() => void invitations.fetchNextPage()} />}
          >
            <For each={data().members}>
              {(member) => (
                <MemberRow
                  member={member}
                  canManage={canManage()}
                  isCurrentUser={isCurrentUser(member)}
                  currentUserRole={me.data?.role}
                  busy={memberRows.busy(memberResourceID(member))}
                  error={memberRows.error(memberResourceID(member))}
                  onRoleChange={changeMemberRole}
                  onRemove={setRemoving}
                />
              )}
            </For>
            <For each={pendingInvitations()}>
              {(invitation) => <InvitationRow invitation={invitation} canManage={canManage()} onRevoke={setRevoking} />}
            </For>
          </DataTable>
        </>}
      </QueryState>

      <Show when={inviting()}>
        <InviteMembersDialog
          currentUserRole={me.data?.role}
          onClose={() => setInviting(false)}
          onInvited={async (results) => {
            setInviting(false);
            setInviteResults(results);
            await invalidateMembers();
          }}
        />
      </Show>

      <Show when={inviteResults()}>
        {(results) => <InvitationResultsDialog results={results()} onClose={() => setInviteResults(null)} />}
      </Show>

      <Show when={removing()}>
        {(member) => (
          <ConfirmDialog
            title="Remove member"
            confirmLabel="Remove"
            busyLabel="Removing..."
            tone="danger"
            target={memberName(member())}
            errorMessage={membersErrorMessage}
            onClose={() => setRemoving(null)}
            onConfirm={async () => {
              await removeMember(memberResourceID(member()));
              await queryClient.invalidateQueries({ queryKey: ["members"] });
            }}
          >
            They lose access to this organization.
          </ConfirmDialog>
        )}
      </Show>

      <Show when={revoking()}>
        {(invitation) => (
          <ConfirmDialog
            title="Revoke invitation"
            confirmLabel="Revoke"
            busyLabel="Revoking..."
            tone="danger"
            target={invitation().email}
            errorMessage={membersErrorMessage}
            onClose={() => setRevoking(null)}
            onConfirm={async () => {
              await revokeInvitation(invitationResourceID(invitation()));
              await queryClient.invalidateQueries({ queryKey: ["invitations"] });
            }}
          >
            The invitation link stops working.
          </ConfirmDialog>
        )}
      </Show>
    </>
  );
}

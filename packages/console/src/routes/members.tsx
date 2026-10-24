import { listGrants, listGroups, memberGrantSources } from "../lib/permissions";
import { MemberAccess } from "../features/access/MemberAccess";
import { useMe } from "../lib/me";
import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { errorMessage, loadError } from "../lib/error";
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
  updateMemberOwnership,
  type InvitationResult,
  type OrganizationInvitation,
  type OrganizationMember,
} from "../lib/members";
import { DataTable, LoadMore, RowActions } from "../ui/DataTable";
import { ConfirmDialog, Dialog, FormDialog } from "../ui/Dialog";
import { Field, TextArea } from "../ui/Field";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { Notice } from "../ui/Notice";
import { QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { Select, type SelectOption } from "../ui/Select";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, toneClass, ui } from "../ui/styles";

type OwnershipChoice = "owner" | "member";
const OWNER_ROLE_OPTION: SelectOption<OwnershipChoice> = { value: "owner", label: "Owner" };

const NON_OWNER_ROLE_OPTIONS: SelectOption<OwnershipChoice>[] = [
  { value: "member", label: "Member" },
];

const OWNER_ROLE_OPTIONS: SelectOption<OwnershipChoice>[] = [OWNER_ROLE_OPTION, ...NON_OWNER_ROLE_OPTIONS];

const EMAIL_FAILED_MESSAGE = "Invitation emails could not be sent. Nobody was invited.";
const NO_EMAIL_DELIVERY = "Email delivery is not set up for this Helmr installation, so members can't be invited.";

const MEMBERS_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage organization members.",
  not_found: "This member or invitation is no longer available.",
  conflict: "This member's role changed. Check it and try again.",
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

function InviteMembersDialog(props: {
  currentUserIsOwner: boolean | undefined;
  onClose: () => void;
  onInvited: (results: InvitationResult[]) => Promise<void>;
}) {
  const [text, setText] = createSignal("");

  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);


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
      const results = await createInvitations({ emails: valid() });
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
      <p class={ui.fieldHint}>Invitations create members without access. An access administrator can assign Roles directly or through Groups after they join. Owners manage ownership separately.</p>
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

// Members and pending invitations in one table: a member's name over their
// email, or an invitee's email; "Since" is when they joined or were invited.
// Roles change from the row menu; your own row has none.
const COLUMNS = ["Member", "Ownership and roles", "Status", "Since", { label: "Actions", srOnly: true }] as const;

function MemberCell(props: { name: string; email?: string | null | undefined; you?: boolean }) {
  return (
    <div class="grid py-1 leading-tight">
      <span class="flex items-center gap-1.5">
        <span class="font-medium text-console-text">{props.name}</span>
        <Show when={props.you}><span class="border border-console-border px-1 font-mono text-micro text-console-muted">You</span></Show>
      </span>
      <Show when={props.email && props.email !== props.name}><span class="text-chrome text-console-muted">{props.email}</span></Show>
    </div>
  );
}

// The roles the signed-in account may give a member. An owner may step down to
// admin on their own row; the server keeps the last owner.
function assignableRoles(currentUserIsOwner: boolean | undefined, ownRow: boolean): SelectOption<OwnershipChoice>[] {
  if (ownRow) return [OWNER_ROLE_OPTION, NON_OWNER_ROLE_OPTIONS[0]!];
  return currentUserIsOwner ? OWNER_ROLE_OPTIONS : NON_OWNER_ROLE_OPTIONS.filter(option=>option.value==="member");
}

function MemberRow(props: {
  member: OrganizationMember;
  canManage: boolean;
  organizationRoles: string[];
  isCurrentUser: boolean;
  currentUserIsOwner: boolean | undefined;
  onInspect: (member: OrganizationMember) => void;
  onChangeRole: (member: OrganizationMember) => void;
  onRemove: (member: OrganizationMember) => void;
}) {
  const id = () => memberResourceID(props.member);
  const isOwner = () => props.member.is_owner;
  const active = () => props.member.status === "active" && props.canManage && id() !== "";
  // Your own row: only an owner has a role to step down from.
  return (
    <>
      <tr>
        <td><MemberCell name={memberName(props.member)} email={props.member.email} you={props.isCurrentUser} /></td>
        <td><Show when={props.member.is_owner}><span class="font-medium">Owner</span></Show><p class="text-control text-console-muted">{props.organizationRoles.join(" · ") || "No organization roles"}</p></td>
        <td><StatusBadge resource="member" status={props.member.status ?? "active"} /></td>
        <td><RelativeTime value={props.member.created_at} /></td>
        <RowActions
          label={`Actions for ${memberName(props.member)}`}
          items={active() ? [
            {label:"Inspect access",onSelect:()=>props.onInspect(props.member)},
            ...(props.currentUserIsOwner ? [{label: "Change ownership", onSelect: () => props.onChangeRole(props.member)}] : []),
            ...(!isOwner() ? [{label: "Remove", tone: "danger" as const, onSelect: () => props.onRemove(props.member)}] : []),
          ] : []}
        />
      </tr>
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
      <td><MemberCell name={props.invitation.email} /></td>
      <td>{"Not an owner"}</td>
      <td><StatusBadge resource="member" status="invited" /></td>
      <td><RelativeTime value={props.invitation.created_at} /></td>
      <RowActions
        label={`Actions for ${props.invitation.email}`}
        items={props.canManage && id() !== "" ? [{ label: "Revoke invitation", tone: "danger", onSelect: () => props.onRevoke(props.invitation) }] : []}
      />
    </tr>
  );
}

function ChangeRoleDialog(props: { member: OrganizationMember; roles: SelectOption<OwnershipChoice>[]; onClose: () => void; onSave: (role: OwnershipChoice) => Promise<void> }) {
  const [role, setRole] = createSignal<OwnershipChoice>((props.member.is_owner ? "owner" : "member"));
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  return (
    <FormDialog
      title="Change ownership"
      intro={<>Change ownership for <strong>{memberName(props.member)}</strong>. Ownership enables ownership transfer and access recovery; it does not grant workload permissions.</>}
      submitLabel="Change ownership"
      busyLabel="Changing..."
      busy={busy()}
      disabled={role() === (props.member.is_owner ? "owner" : "member")}
      error={error()}
      onClose={props.onClose}
      onSubmit={async () => {
        setBusy(true); setError(null);
        try { await props.onSave(role()); props.onClose(); }
        catch (cause) { setError(membersErrorMessage(cause)); }
        finally { setBusy(false); }
      }}
    >
      <Field label="Ownership">
        <Select<OwnershipChoice> value={role()} options={props.roles} onChange={setRole} minWidth="100%" />
      </Field>
    </FormDialog>
  );
}

export function Members() {
  const queryClient = useQueryClient();
  const [inspecting,setInspecting]=createSignal<OrganizationMember>();
  const [inviting, setInviting] = createSignal(false);
  const [inviteResults, setInviteResults] = createSignal<InvitationResult[] | null>(null);
  const [removing, setRemoving] = createSignal<OrganizationMember | null>(null);
  const [revoking, setRevoking] = createSignal<OrganizationInvitation | null>(null);

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

  const canManage = createMemo(() => me.data?.permissions.includes("access.manage") === true);
  const grants = createQuery(()=>({queryKey:["access","grants"],queryFn:listGrants,enabled:canManage()}));
  const groups = createQuery(()=>({queryKey:["access","groups"],queryFn:listGroups,enabled:canManage()}));
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

  // The member whose role is being changed, read fresh from the list so a
  // retry after someone else's change expects the role they set.
  const [changingRoleID, setChangingRoleID] = createSignal<string | null>(null);
  const changingRole = () => members.data?.members.find((member) => memberResourceID(member) === changingRoleID()) ?? null;
  const changeOwnershipChoice = async (member: OrganizationMember, role: OwnershipChoice) => {
    const id = memberResourceID(member);
    if (!id || (role === "owner") === member.is_owner) return;
    try {
      await updateMemberOwnership(id, role === "owner", member.is_owner);
    } finally {
      await queryClient.invalidateQueries({ queryKey: ["members"] });
    }
  };

  const isCurrentUser = (member: OrganizationMember) =>
    member.membership_id === me.data?.membership_id;

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
        <Notice tone="info" class="mb-3">Manage access permission is required to view and manage members.</Notice>
      </Show>
      <Show when={canManage() && !emailDelivery()}>
        <Notice tone="info" class="mb-3">{NO_EMAIL_DELIVERY}</Notice>
      </Show>

      <QueryState
        query={members}
        pending={<TableSkeleton label="Loading members" columns={COLUMNS} minWidth="min-w-160" />}
        error={(error) => loadError(error, "Could not load members.", MEMBERS_ERROR_MESSAGES)}
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
            minWidth="min-w-160"
            footer={<LoadMore count={data().members.length} noun={{ one: "member", other: "members" }} extra={{ count: pendingInvitations().length, noun: { one: "invitation", other: "invitations" } }} hasMore={invitations.hasNextPage} loading={invitations.isFetchingNextPage} failed={invitations.isFetchNextPageError} onLoadMore={() => void invitations.fetchNextPage()} />}
          >
            <For each={data().members}>
              {(member) => (
                <MemberRow
                  member={member}
                  canManage={canManage()}
                  organizationRoles={member.status === "active" ? [...new Set(memberGrantSources(grants.data?.grants ?? [], groups.data?.groups ?? [], member.membership_id).filter(g=>g.target_kind === "organization").map(g=>g.role_name))] : []}
                  isCurrentUser={isCurrentUser(member)}
                  currentUserIsOwner={me.data?.is_owner}
                  onInspect={setInspecting}
                  onChangeRole={(member) => setChangingRoleID(memberResourceID(member))}
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
          currentUserIsOwner={me.data?.is_owner}
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

      <Show when={inspecting()}>{member=><Dialog title={`Access: ${memberName(member())}`} onClose={()=>setInspecting(undefined)}><MemberAccess membershipID={member().membership_id} /></Dialog>}</Show>
      <Show when={changingRole()}>
        {(member) => <ChangeRoleDialog member={member()} roles={assignableRoles(me.data?.is_owner, isCurrentUser(member()))} onClose={() => setChangingRoleID(null)} onSave={(role) => changeOwnershipChoice(member(), role)} />}
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

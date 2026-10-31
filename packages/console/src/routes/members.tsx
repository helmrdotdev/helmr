import { listGrants, listGroups, memberGrantSources, memberHref } from "../lib/permissions";
import { GrantAccessDialog } from "../features/access/GrantAccessDialog";
import type { ActionMenuItem } from "../ui/ActionMenu";
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
  changeMemberAccess,
  revokeInvitation,
  type InvitationResult,
  type OrganizationInvitation,
  type OrganizationMember,
} from "../lib/members";
import { DataTable, EmptyCell, LoadMore, PrimaryLink, RowActions } from "../ui/DataTable";
import { Tag } from "../ui/Tag";
import { ConfirmDialog, Dialog, FormDialog } from "../ui/Dialog";
import { Field, TextArea } from "../ui/Field";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { Notice } from "../ui/Notice";
import { QueryState, DeniedState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, toneClass, ui } from "../ui/styles";
import { useAuditItem } from "../features/audit/auditLink";

const EMAIL_FAILED_MESSAGE = "Invitation emails could not be sent. Nobody was invited.";
const NO_EMAIL_DELIVERY = "Email delivery is not set up in Helmr, so members can't be invited.";

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
  email_failed: "Invitation email could not be sent",
};

function invitationFailureReason(result: InvitationResult): string {
  return INVITATION_FAILURE_REASONS[result.error?.code ?? ""] ?? "Invitation could not be created";
}

function memberName(member: OrganizationMember): string {
  return member.display_name || member.email || member.user_id;
}

function InviteMembersDialog(props: {
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
      // Each address is invited on its own. When no email could be sent nothing
      // was created: keep the form and its addresses so they can be sent again.
      if (results.every((result) => result.error?.code === "email_failed")) {
        setError(EMAIL_FAILED_MESSAGE);
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
      busyLabel="Inviting…"
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
      <p class={ui.fieldHint}>Invitations create members without access. An access administrator can grant roles directly or through groups after they join. Owners manage ownership separately.</p>
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
            <p class="m-0 text-body leading-normal text-console-text">Could not invite {failed().length}:</p>
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

// Members and pending invitations in one table: a member's name, or an
// invitee's email; "Since" is when they joined or were invited. Ownership and
// your own row are tags after the name.
const COLUMNS = ["Member", "Email", "Organization roles", "Status", "Since", { label: "Actions", srOnly: true }] as const;

function MemberCell(props: { name: string; owner?: boolean; you?: boolean; href?: string | undefined }) {
  return (
    <span class="inline-flex items-center gap-1.5">
      <Show when={props.href} fallback={<span class="font-mono text-chrome text-console-text">{props.name}</span>}>{href => <PrimaryLink href={href()}>{props.name}</PrimaryLink>}</Show>
      <Show when={props.owner}><Tag title="Can appoint owners and recover access administration">owner</Tag></Show>
      <Show when={props.you}><Tag>you</Tag></Show>
    </span>
  );
}

function MemberRow(props: {
  member: OrganizationMember;
  canManage: boolean;
  canRemove: boolean;
  canGrant: boolean;
  organizationRoles: string[];
  isCurrentUser: boolean;
  currentUserIsOwner: boolean;
  onRemove: (member: OrganizationMember) => void;
  onAccess: (member: OrganizationMember, action: ConfirmedAccessChange) => void;
  onResume: (member: OrganizationMember) => void;
  onGrant: (member: OrganizationMember) => void;
}) {
  const id = () => memberResourceID(props.member);
  const isOwner = () => props.member.is_owner;
  const active = () => props.member.status !== "removed" && props.canManage && id() !== "";
  const auditItem = useAuditItem();
  return (
    <>
      <tr>
        <td><MemberCell name={memberName(props.member)} owner={props.member.is_owner} you={props.isCurrentUser} href={props.member.status === "active" && props.canManage ? memberHref(props.member.membership_id) : undefined} /></td>
        <td class="text-console-muted!">{props.member.email && props.member.email !== memberName(props.member) ? props.member.email : <EmptyCell />}</td>
        <td class="max-w-64" title={props.organizationRoles.join(", ") || undefined}><Show when={props.organizationRoles.length} fallback={<EmptyCell />}><span class="flex min-w-0 items-center gap-1.5"><span class="min-w-0 truncate">{props.organizationRoles[0]}</span><Show when={props.organizationRoles.length > 1}><span aria-hidden="true"><Tag title={props.organizationRoles.slice(1).join(", ")}>+{props.organizationRoles.length - 1}</Tag></span><span class="sr-only">, {props.organizationRoles.slice(1).join(", ")}</span></Show></span></Show></td>
        <td><StatusBadge resource="member" status={props.member.status ?? "active"} /></td>
        <td><RelativeTime value={props.member.created_at} /></td>
        <RowActions
          label={`Actions for ${memberName(props.member)}`}
          items={active() ? memberMenu({
            grant: props.canGrant ? () => props.onGrant(props.member) : undefined,
            ownership: props.currentUserIsOwner && props.member.status === "active",
            remove: !isOwner() && props.canRemove ? () => props.onRemove(props.member) : undefined,
          }).concat(props.canRemove ? [
 {label:"Revoke sessions",onSelect:()=>props.onAccess(props.member,"revoke-sessions")},
 ...(!isOwner() ? [props.member.status === "suspended" ? {label:"Resume member",onSelect:()=>props.onResume(props.member)} : {label:"Suspend member",onSelect:()=>props.onAccess(props.member,"suspend")}] : []),
 ] : []).concat([{label:"View related work",href:`/sessions?user_id=${encodeURIComponent(props.member.user_id)}`}],auditItem({ actor: props.member.user_id })) : [{label:"View related work",href:`/sessions?user_id=${encodeURIComponent(props.member.user_id)}`},...auditItem({ actor: props.member.user_id })]}
        />
      </tr>
    </>
  );
}

// Revoking sessions and suspending both end the member's current access here;
// each says what it ends and what stays.
type ConfirmedAccessChange = "revoke-sessions" | "suspend";
const ACCESS_CHANGES: Record<ConfirmedAccessChange, { title: string; busyLabel: string; body: string }> = {
  "revoke-sessions": {
    title: "Revoke sessions",
    busyLabel: "Revoking…",
    body: "They are signed out of this organization and must sign in again. Their sign-in sessions, CLI sign-ins they approved and linked Slack identities here are revoked. They stay a member with the same roles and groups; other organizations are unaffected. Accepted work continues; use separate work controls to stop it.",
  },
  suspend: {
    title: "Suspend member",
    busyLabel: "Suspending…",
    body: "They are signed out of this organization and cannot use it until resumed, after which they sign in again. Their sign-in sessions, CLI sign-ins they approved and linked Slack identities here are revoked. Roles and groups are kept; other organizations are unaffected. Accepted work continues; use separate work controls to stop it.",
  },
};

function InvitationRow(props: {
  invitation: OrganizationInvitation;
  canManage: boolean;
  onRevoke: (invitation: OrganizationInvitation) => void;
}) {
  const id = () => invitationResourceID(props.invitation);
  return (
    <tr>
      <td><MemberCell name={props.invitation.email} /></td>
      <td><EmptyCell /></td>
      <td><EmptyCell /></td>
      <td><StatusBadge resource="member" status="invited" /></td>
      <td><RelativeTime value={props.invitation.created_at} /></td>
      <RowActions
        label={`Actions for ${props.invitation.email}`}
        items={props.canManage && id() !== "" ? [{ label: "Revoke invitation", tone: "danger", onSelect: () => props.onRevoke(props.invitation) }] : []}
      />
    </tr>
  );
}

// The object menu for a member, the same in its list row and by its name.
// Ownership is changed on the Ownership page, which only owners can open.
export function memberMenu(options: { grant?: (() => void) | undefined; ownership?: boolean | undefined; remove?: (() => void) | undefined }): ActionMenuItem[] {
  return [
    ...(options.grant ? [{ label: "Grant access", onSelect: options.grant }] : []),
    ...(options.ownership ? [{ label: "Manage ownership", href: "/settings/ownership" }] : []),
    ...(options.remove ? [{ label: "Remove member", tone: "danger" as const, onSelect: options.remove }] : []),
  ];
}

export function RemoveMemberDialog(props: { member: OrganizationMember; onClose: () => void; onRemoved: () => Promise<void> | void }) {
  return (
    <ConfirmDialog title="Remove member" confirmLabel="Remove" busyLabel="Removing…" tone="danger" target={memberName(props.member)} errorMessage={membersErrorMessage} onClose={props.onClose}
      onConfirm={async () => { await removeMember(memberResourceID(props.member)); await props.onRemoved(); }}>
      They lose access to this organization.
    </ConfirmDialog>
  );
}

export function Members() {
  const queryClient = useQueryClient();
  const [inviting, setInviting] = createSignal(false);
  const [inviteResults, setInviteResults] = createSignal<InvitationResult[] | null>(null);
  const [accessChange,setAccessChange]=createSignal<{member:OrganizationMember;action:ConfirmedAccessChange}>();
  const [resumeError, setResumeError] = createSignal<string>();
  const [removing, setRemoving] = createSignal<OrganizationMember | null>(null);
  const [granting, setGranting] = createSignal<OrganizationMember>();
  const [revoking, setRevoking] = createSignal<OrganizationInvitation | null>(null);

  const me = useMe();
  const members = createQuery(() => ({
    queryKey: ["members"],
    queryFn: listMembers,
    enabled: me.data?.permissions.includes("members.read") === true,
    retry: false,
  }));
  const invitations = createInfiniteQuery(() => ({
    queryKey: ["invitations"],
    enabled: me.data?.permissions.includes("invitations.read") === true,
    queryFn: ({ pageParam }) => listInvitations(pageParam || undefined),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    retry: false,
  }));

  const can = (permission: string) => me.data?.permissions.includes(permission) === true;
  const canManage = () => can("members.read");
  const grants = createQuery(()=>({queryKey:["access","grants"],queryFn:listGrants,enabled:can("assignments.read")}));
  const groups = createQuery(()=>({queryKey:["access","groups"],queryFn:listGroups,enabled:can("groups.read")}));
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

  // A suspended member has no live access left to end, so resuming needs no
  // confirmation; they sign in again afterwards.
  const resume = async (member: OrganizationMember) => {
    setResumeError(undefined);
    try {
      await changeMemberAccess(member.membership_id, "resume");
      await queryClient.invalidateQueries({ queryKey: ["members"] });
    } catch (cause) {
      setResumeError(membersErrorMessage(cause));
    }
  };

  const isCurrentUser = (member: OrganizationMember) =>
    member.membership_id === me.data?.membership_id;

  return (
    <>
      <PageHeader
        title="Members"
        actions={
          <Show when={can("invitations.create")}>
            <button class={ui.button} type="button" disabled={!emailDelivery()} onClick={() => setInviting(true)}>
              Invite members
            </button>
          </Show>
        }
      />

      <Show when={!me.isPending && !canManage()}>
        <div class="mb-3"><DeniedState message="You need Read members permission to view members." /></div>
      </Show>
      <Show when={resumeError()}>{message => <Notice tone="danger" role="alert" class="mb-3">{message()}</Notice>}</Show>
      <Show when={can("invitations.create") && !emailDelivery()}>
        <Notice tone="info" class="mb-3">{NO_EMAIL_DELIVERY}</Notice>
      </Show>

      <Show when={canManage()}>
        <QueryState query={members} pending={<TableSkeleton label="Loading members" columns={COLUMNS} minWidth="min-w-160" />} error={error => loadError(error, "Could not load members.", MEMBERS_ERROR_MESSAGES)}>{() => null}</QueryState>
      </Show>
      <Show when={can("invitations.read")}>
        <QueryState query={invitations} pending={<TableSkeleton label="Loading invitations" columns={COLUMNS} minWidth="min-w-160" />} error={error => loadError(error, "Could not load invitations.")}>{() => null}</QueryState>
      </Show>
      <Show when={(canManage() && members.data) || (can("invitations.read") && invitations.data)}>
          <DataTable
            columns={COLUMNS}
            minWidth="min-w-160"
            footer={<LoadMore count={canManage() ? members.data?.members.length ?? 0 : 0} noun={{ one: "member", other: "members" }} extra={{ count: pendingInvitations().length, noun: { one: "invitation", other: "invitations" } }} hasMore={invitations.hasNextPage} loading={invitations.isFetchingNextPage} failed={invitations.isFetchNextPageError} onLoadMore={() => void invitations.fetchNextPage()} />}
          >
            <For each={canManage() ? members.data?.members ?? [] : []}>
              {(member) => (
                <MemberRow
                  member={member}
                  canManage={canManage()}
                  canRemove={can("members.remove")}
                  canGrant={can("assignments.create")}
                  organizationRoles={member.status === "active" ? [...new Set(memberGrantSources(grants.data?.grants ?? [], groups.data?.groups ?? [], member.membership_id).filter(g=>g.target_kind === "organization").map(g=>g.role_name))] : []}
                  isCurrentUser={isCurrentUser(member)}
                  currentUserIsOwner={me.data?.is_owner === true}
                  onRemove={(member) => { setResumeError(undefined); setRemoving(member); }}
                  onAccess={(member,action)=>{setResumeError(undefined);setAccessChange({member,action});}}
                  onResume={(member) => void resume(member)}
                  onGrant={(member) => { setResumeError(undefined); setGranting(member); }}
                />
              )}
            </For>
            <For each={pendingInvitations()}>
              {(invitation) => <InvitationRow invitation={invitation} canManage={can("invitations.revoke")} onRevoke={setRevoking} />}
            </For>
          </DataTable>
      </Show>

      <Show when={inviting()}>
        <InviteMembersDialog
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

      <Show when={accessChange()}>{change => <ConfirmDialog title={ACCESS_CHANGES[change().action].title} confirmLabel={ACCESS_CHANGES[change().action].title} busyLabel={ACCESS_CHANGES[change().action].busyLabel} tone="danger" target={memberName(change().member)} errorMessage={membersErrorMessage} onClose={()=>setAccessChange(undefined)} onConfirm={async()=>{await changeMemberAccess(change().member.membership_id,change().action);await queryClient.invalidateQueries({queryKey:["members"]});}}>{ACCESS_CHANGES[change().action].body}</ConfirmDialog>}</Show>
      <Show when={removing()}>
        {(member) => <RemoveMemberDialog member={member()} onClose={() => setRemoving(null)} onRemoved={() => queryClient.invalidateQueries({ queryKey: ["members"] })} />}
      </Show>
      <Show when={granting()} keyed>{member => <GrantAccessDialog recipient={{ kind: "member", id: member.membership_id, name: memberName(member) }} onClose={() => setGranting(undefined)} />}</Show>

      <Show when={revoking()}>
        {(invitation) => (
          <ConfirmDialog
            title="Revoke invitation"
            confirmLabel="Revoke"
            busyLabel="Revoking…"
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

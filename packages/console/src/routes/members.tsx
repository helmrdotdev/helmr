import { isOrganizationAdmin, useMe } from "../lib/me";
import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { errorMessage } from "../lib/error";
import {
  createInvitation,
  invitationResourceID,
  listInvitations,
  listMembers,
  memberResourceID,
  removeMember,
  revokeInvitation,
  updateMemberRole,
  type MemberRole,
  type OrganizationInvitation,
  type OrganizationMember,
} from "../lib/members";
import { DataTable, EmptyCell, LoadMore, RowActions, RowError } from "../ui/DataTable";
import { ConfirmDialog, FormDialog, RevealDialog } from "../ui/Dialog";
import { Field, TextInput } from "../ui/Field";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { Notice } from "../ui/Notice";
import { QueryState } from "../ui/QueryState";
import { Select, type SelectOption } from "../ui/Select";
import { StatusBadge } from "../ui/StatusBadge";
import { createRowActions } from "../ui/rowActions";
import { ui } from "../ui/styles";

const OWNER_ROLE_OPTION: SelectOption<MemberRole> = { value: "owner", label: "Owner" };

const NON_OWNER_ROLE_OPTIONS: SelectOption<MemberRole>[] = [
  { value: "admin", label: "Admin" },
  { value: "developer", label: "Developer" },
  { value: "viewer", label: "Viewer" },
];

const OWNER_ROLE_OPTIONS: SelectOption<MemberRole>[] = [OWNER_ROLE_OPTION, ...NON_OWNER_ROLE_OPTIONS];

const MEMBERS_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage organization members.",
  invalid_email: "Enter a valid email address.",
  already_member: "That email address is already a member.",
  not_found: "This member or invitation is no longer available.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

const membersErrorMessage = (error: unknown) => errorMessage(error, INTERNAL_ERROR_MESSAGE, MEMBERS_ERROR_MESSAGES);

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

function validInviteEmail(value: string): boolean {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value.trim());
}

function CreateInviteDialog(props: {
  currentUserRole: string | null | undefined;
  onClose: () => void;
  onCreated: (inviteURL: string) => Promise<void>;
}) {
  const [email, setEmail] = createSignal("");
  const [role, setRole] = createSignal<MemberRole>("developer");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const roleOptions = createMemo(() =>
    props.currentUserRole === "owner" ? OWNER_ROLE_OPTIONS : NON_OWNER_ROLE_OPTIONS,
  );

  const submit = async () => {
    setError(null);
    if (!validInviteEmail(email())) {
      setError(MEMBERS_ERROR_MESSAGES["invalid_email"] ?? INTERNAL_ERROR_MESSAGE);
      return;
    }
    setSubmitting(true);
    try {
      const result = await createInvitation({ email: email().trim(), role: role() });
      await props.onCreated(result.invite_url);
    } catch (createError) {
      setError(membersErrorMessage(createError));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <FormDialog
      title="Invite member"
      intro="Create an email invitation for this organization."
      submitLabel="Create invite"
      busyLabel="Creating..."
      busy={submitting()}
      disabled={email().trim() === ""}
      error={error()}
      onSubmit={() => void submit()}
      onClose={props.onClose}
    >
      <Field label="Email">
        <TextInput type="email" value={email()} autocomplete="email" onInput={(event) => setEmail(event.currentTarget.value)} autofocus />
      </Field>
      <Field label="Role">
        <Select<MemberRole> value={role()} options={roleOptions()} onChange={setRole} minWidth="100%" />
      </Field>
    </FormDialog>
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
  const [inviteURL, setInviteURL] = createSignal<string | null>(null);
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
            <button class={ui.button} type="button" onClick={() => setInviting(true)}>
              Invite member
            </button>
          </Show>
        }
      />

      <Show when={!me.isPending && !canManage()}>
        <Notice tone="info" class="mb-3">Member settings require admin access.</Notice>
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
        <CreateInviteDialog
          currentUserRole={me.data?.role}
          onClose={() => setInviting(false)}
          onCreated={async (url) => {
            setInviting(false);
            setInviteURL(url);
            await invalidateMembers();
          }}
        />
      </Show>

      <Show when={inviteURL()}>
        {(url) => (
          <RevealDialog
            title="Invite created"
            intro="Share this invitation link with the new member."
            value={url()}
            warning="Copy this link now. It is not shown again."
            onClose={() => setInviteURL(null)}
          />
        )}
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

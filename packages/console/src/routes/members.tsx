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
import { ActionMenu } from "../ui/ActionMenu";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Dialog } from "../ui/Dialog";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { Notice } from "../ui/Notice";
import { Section } from "../ui/Page";
import { Select, type SelectOption } from "../ui/Select";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { useClipboard } from "../ui/useClipboard";
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

function memberEmail(member: OrganizationMember): string {
  return member.email ?? "-";
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

function CreateInviteModal(props: {
  currentUserRole: string | null | undefined;
  onClose: () => void;
  onCreated: () => Promise<void>;
}) {
  const [email, setEmail] = createSignal("");
  const [role, setRole] = createSignal<MemberRole>("developer");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [inviteURL, setInviteURL] = createSignal<string | null>(null);
  const clipboard = useClipboard();

  const closeAndClear = () => {
    setEmail("");
    setRole("developer");
    setInviteURL(null);
    setError(null);
    props.onClose();
  };

  const roleOptions = createMemo(() =>
    props.currentUserRole === "owner" ? OWNER_ROLE_OPTIONS : NON_OWNER_ROLE_OPTIONS,
  );

  const submit = async (event: Event) => {
    event.preventDefault();
    setError(null);
    if (!validInviteEmail(email())) {
      setError(MEMBERS_ERROR_MESSAGES["invalid_email"] ?? INTERNAL_ERROR_MESSAGE);
      return;
    }
    setSubmitting(true);
    try {
      const result = await createInvitation({
        email: email().trim(),
        role: role(),
      });
      setInviteURL(result.invite_url);
      await props.onCreated();
    } catch (createError) {
      setError(membersErrorMessage(createError));
    } finally {
      setSubmitting(false);
    }
  };

  const copyInviteURL = async () => {
    const url = inviteURL();
    if (url) await clipboard.copy(url);
  };

  return (
    <Dialog
      title={inviteURL() ? "Invite created" : "Invite member"}
      onClose={closeAndClear}
      closeDisabled={submitting()}
    >
      <Show when={inviteURL()} keyed fallback={
        <form onSubmit={submit}>
          <p class={ui.modalIntro}>Create an email invitation for this organization.</p>
          <label class={ui.field}>
            <span>Email</span>
            <input
              class={ui.input}
              type="email"
              value={email()}
              autocomplete="email"
              onInput={(event) => setEmail(event.currentTarget.value)}
              autofocus
            />
          </label>
          <label class={ui.field}>
            <span>Role</span>
            <Select<MemberRole>
              value={role()}
              options={roleOptions()}
              onChange={setRole}
              ariaLabel="Invitation role"
              minWidth="100%"
            />
          </label>
          <Show when={error()}>
            <p class={ui.error} role="alert">{error()}</p>
          </Show>
          <div class={ui.modalActions}>
            <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={closeAndClear}>
              Cancel
            </button>
            <button class={ui.button} type="submit" disabled={submitting() || email().trim() === ""}>
              {submitting() ? "Creating..." : "Create invite"}
            </button>
          </div>
        </form>
      }>
        {(url) => (
          <>
            <p class={ui.modalIntro}>Share this invitation link with the new member.</p>
            <code class={ui.rawKey}>{url}</code>
            <div class={ui.modalActions}>
              <button type="button" class={ui.secondaryButton} onClick={closeAndClear}>Done</button>
              <button class={ui.button} type="button" onClick={copyInviteURL}>Copy link</button>
            </div>
            <Show when={clipboard.state() !== "idle"}>
              <p class={ui.inlineState} role="status">{clipboard.state() === "copied" ? "Copied" : "Copy failed"}</p>
            </Show>
          </>
        )}
      </Show>
    </Dialog>
  );
}

function MemberRow(props: {
  member: OrganizationMember;
  canManage: boolean;
  isCurrentUser: boolean;
  currentUserRole: string | null | undefined;
  busy: (action: "role" | "remove") => boolean;
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
  const busy = (action: "role" | "remove") => props.busy(action);
  return (
    <tr>
      <td><strong class="font-medium text-console-text">{memberName(props.member)}</strong></td>
      <td><IDText value={props.member.user_id} /></td>
      <td>{memberEmail(props.member)}</td>
      <td>
        <Show when={canChangeRole()} fallback={<StatusBadge resource="role" status={props.member.role} />}>
          <Select<MemberRole>
            value={props.member.role}
            options={roleOptions()}
            onChange={(nextRole) => props.onRoleChange(props.member, nextRole)}
            disabled={busy("role")}
            ariaLabel={`Role for ${memberName(props.member)}`}
            minWidth="132px"
          />
        </Show>
      </td>
      <td><StatusBadge resource="member" status={props.member.status ?? "active"} /></td>
      <td><RelativeTime value={props.member.created_at} /></td>
      <td class={ui.actionsCell}>
        <Show when={canRemove()} fallback={<span class={ui.muted}>No actions</span>}>
          <ActionMenu
            label={`Actions for ${memberName(props.member)}`}
            items={[{
              label: "Remove",
              busyLabel: busy("remove") ? "Removing..." : undefined,
              disabled: busy("remove"),
              tone: "danger",
              onSelect: () => props.onRemove(props.member),
            }]}
          />
        </Show>
        <Show when={props.error}>
          <p class={ui.rowError} role="alert">{props.error}</p>
        </Show>
      </td>
    </tr>
  );
}

function InvitationRow(props: {
  invitation: OrganizationInvitation;
  canManage: boolean;
  busy: boolean;
  error: string | null;
  onRevoke: (invitation: OrganizationInvitation) => void;
}) {
  const id = () => invitationResourceID(props.invitation);
  const busy = () => props.busy;
  return (
    <tr>
      <td><strong class="font-medium text-console-text">{props.invitation.email}</strong></td>
      <td><StatusBadge resource="role" status={props.invitation.role} /></td>
      <td><StatusBadge resource="invitation" status={props.invitation.status ?? "pending"} /></td>
      <td><RelativeTime value={props.invitation.expires_at} /></td>
      <td class={ui.actionsCell}>
        <Show when={props.canManage} fallback={<span class={ui.muted}>No actions</span>}>
          <ActionMenu
            label={`Actions for ${props.invitation.email}`}
            items={[{
              label: "Revoke",
              busyLabel: busy() ? "Revoking..." : undefined,
              disabled: id() === "" || busy(),
              tone: "danger",
              onSelect: () => props.onRevoke(props.invitation),
            }]}
          />
        </Show>
        <Show when={props.error}>
          <p class={ui.rowError} role="alert">{props.error}</p>
        </Show>
      </td>
    </tr>
  );
}

export function Members() {
  const queryClient = useQueryClient();
  const [modalOpen, setModalOpen] = createSignal(false);
  const memberRows = createRowActions<"role" | "remove">();
  const invitationRows = createRowActions<"revoke">();

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

  const remove = async (member: OrganizationMember) => {
    const id = memberResourceID(member);
    if (!id) return;
    if (!window.confirm(`Remove ${memberName(member)} from this organization?`)) return;
    await memberRows.run(id, "remove", async () => {
      await removeMember(id);
      await queryClient.invalidateQueries({ queryKey: ["members"] });
    }, membersErrorMessage);
  };

  const revoke = async (invitation: OrganizationInvitation) => {
    const id = invitationResourceID(invitation);
    if (!id) return;
    if (!window.confirm(`Revoke invitation for ${invitation.email}?`)) return;
    await invitationRows.run(id, "revoke", async () => {
      await revokeInvitation(id);
      await queryClient.invalidateQueries({ queryKey: ["invitations"] });
    }, membersErrorMessage);
  };

  const isCurrentUser = (member: OrganizationMember) =>
    member.user_id === me.data?.user_id || memberResourceID(member) === me.data?.user_id;

  return (
    <>
      <PageHeader
        title="Members"
        subtitle="Organization members, roles, and pending email invitations."
        actions={
          <Show when={canManage()}>
            <button class={ui.button} type="button" onClick={() => setModalOpen(true)}>
              Invite member
            </button>
          </Show>
        }
      />

      <Show when={!me.isPending && !canManage()}>
        <Notice tone="info" class="mb-3">Member settings require admin access.</Notice>
      </Show>

      <Show when={members.isError}>
        <StatePanel error={membersErrorMessage(members.error)} />
      </Show>

      <div class={ui.sectionStack}>
      <Section
        title="Current members"
        count={members.data?.members.length}
        description="Active and disabled accounts currently associated with this organization."
      >
        <Show when={!members.isPending} fallback={<StatePanel loading="Loading members..." />}>
          <Show when={(members.data?.members.length ?? 0) > 0} fallback={<StatePanel empty="No members found." />}>
            <DataTable columns={["Member", "User ID", "Email", "Role", "Status", "Joined", { label: "Actions", srOnly: true }]} minWidth="min-w-245">
              <For each={members.data?.members ?? []}>
                {(member) => (
                  <MemberRow
                    member={member}
                    canManage={canManage()}
                    isCurrentUser={isCurrentUser(member)}
                    currentUserRole={me.data?.role}
                    busy={(action) => memberRows.busy(memberResourceID(member), action)}
                    error={memberRows.error(memberResourceID(member))}
                    onRoleChange={changeMemberRole}
                    onRemove={remove}
                  />
                )}
              </For>
            </DataTable>
          </Show>
        </Show>
      </Section>

      <Section
        title="Pending invitations"
        count={pendingInvitations().length}
        description="Email invitations that have not been accepted yet."
      >

        <Show when={invitations.isError}>
          <StatePanel error={membersErrorMessage(invitations.error)} />
        </Show>

        <Show when={!invitations.isPending} fallback={<StatePanel loading="Loading invitations..." />}>
          <Show when={pendingInvitations().length > 0} fallback={<StatePanel empty="No pending invitations." />}>
            <DataTable columns={["Email", "Role", "Status", "Expires", { label: "Actions", srOnly: true }]} minWidth="min-w-225">
              <For each={pendingInvitations()}>
                {(invitation) => (
                  <InvitationRow
                    invitation={invitation}
                    canManage={canManage()}
                    busy={invitationRows.busy(invitationResourceID(invitation))}
                    error={invitationRows.error(invitationResourceID(invitation))}
                    onRevoke={revoke}
                  />
                )}
              </For>
            </DataTable>
            <Show when={invitations.hasNextPage}>
              <div class={ui.actionRow}>
                <button
                  type="button"
                  class={ui.secondaryButton}
                  disabled={invitations.isFetchingNextPage}
                  onClick={() => void invitations.fetchNextPage()}
                >
                  {invitations.isFetchingNextPage ? "Loading..." : "Load more invitations"}
                </button>
              </div>
            </Show>
          </Show>
        </Show>
      </Section>
      </div>

      <Show when={modalOpen()}>
        <CreateInviteModal
          onClose={() => setModalOpen(false)}
          onCreated={invalidateMembers}
          currentUserRole={me.data?.role}
        />
      </Show>
    </>
  );
}

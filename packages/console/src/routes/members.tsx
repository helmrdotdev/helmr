import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { getMe } from "../lib/auth";
import {
  createInvitation,
  invitationResourceID,
  listInvitations,
  listMembers,
  memberResourceID,
  removeMember,
  revokeInvitation,
  updateMemberRole,
  type InvitationStatus,
  type MemberRole,
  type MemberStatus,
  type OrganizationInvitation,
  type OrganizationMember,
} from "../lib/members";
import { ActionMenu } from "../ui/ActionMenu";
import { Modal } from "../ui/Modal";
import { Select, type SelectOption } from "../ui/Select";
import { cx, statusBadgeClass, ui } from "../ui/styles";

const OWNER_ROLE_OPTION: SelectOption<MemberRole> = { value: "owner", label: "Owner" };

const NON_OWNER_ROLE_OPTIONS: SelectOption<MemberRole>[] = [
  { value: "admin", label: "Admin" },
  { value: "developer", label: "Developer" },
  { value: "viewer", label: "Viewer" },
];

const OWNER_ROLE_OPTIONS: SelectOption<MemberRole>[] = [OWNER_ROLE_OPTION, ...NON_OWNER_ROLE_OPTIONS];

const ROLE_LABELS: Record<MemberRole, string> = {
  owner: "Owner",
  admin: "Admin",
  developer: "Developer",
  viewer: "Viewer",
};

const MEMBER_STATUS_LABELS: Record<MemberStatus, string> = {
  active: "Active",
  disabled: "Disabled",
  pending: "Pending",
};

const INVITATION_STATUS_LABELS: Record<InvitationStatus, string> = {
  pending: "Pending",
  accepted: "Accepted",
  revoked: "Revoked",
  expired: "Expired",
};

const MEMBERS_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage organization members.",
  invalid_email: "Enter a valid email address.",
  invalid_role: "Choose admin, developer, or viewer.",
  already_member: "That email address is already a member.",
  invitation_exists: "There is already a pending invitation for that email address.",
  not_found: "This member or invitation is no longer available.",
  internal: "Something went wrong. Please try again.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

function membersErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    return MEMBERS_ERROR_MESSAGES[error.errorKind] ?? error.message ?? INTERNAL_ERROR_MESSAGE;
  }
  return INTERNAL_ERROR_MESSAGE;
}

function formatDateTime(value?: string | null): string {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "-";
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

function formatExpiration(value?: string | null): string {
  if (!value) return "-";
  return `${formatDateTime(value)} (${formatRelative(value)})`;
}

function memberName(member: OrganizationMember): string {
  return member.display_name || member.email || member.user_id;
}

function memberEmail(member: OrganizationMember): string {
  return member.email ?? "-";
}

function memberJoinedAt(member: OrganizationMember): string {
  return formatDateTime(member.created_at);
}

function canManageFromMe(role?: string | null, permissions?: string[]): boolean {
  if (role === "owner" || role === "admin") return true;
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

function RoleBadge(props: { role: MemberRole }) {
  return <span class={statusBadgeClass(props.role === "owner" ? "waiting" : "expired")}>{ROLE_LABELS[props.role]}</span>;
}

function MemberStatusBadge(props: { status?: MemberStatus | undefined }) {
  const status = () => props.status ?? "active";
  const tone = (): "succeeded" | "waiting" | "revoked" => {
    if (status() === "disabled") return "revoked";
    if (status() === "pending") return "waiting";
    return "succeeded";
  };
  return <span class={statusBadgeClass(tone())}>{MEMBER_STATUS_LABELS[status()] ?? status()}</span>;
}

function InvitationStatusBadge(props: { status?: InvitationStatus | undefined }) {
  const status = () => props.status ?? "pending";
  const tone = (): "succeeded" | "waiting" | "revoked" | "expired" => {
    if (status() === "accepted") return "succeeded";
    if (status() === "pending") return "waiting";
    if (status() === "expired") return "expired";
    return "revoked";
  };
  return <span class={statusBadgeClass(tone())}>{INVITATION_STATUS_LABELS[status()] ?? status()}</span>;
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
  const [copyState, setCopyState] = createSignal<string | null>(null);

  const closeAndClear = () => {
    setEmail("");
    setRole("developer");
    setInviteURL(null);
    setCopyState(null);
    setError(null);
    props.onClose();
  };

  const roleOptions = createMemo(() =>
    props.currentUserRole === "owner" ? OWNER_ROLE_OPTIONS : NON_OWNER_ROLE_OPTIONS,
  );

  const submit = async (event: Event) => {
    event.preventDefault();
    setError(null);
    setCopyState(null);
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
    if (!url) return;
    try {
      await navigator.clipboard.writeText(url);
      setCopyState("Copied");
    } catch {
      setCopyState("Copy failed");
    }
  };

  return (
    <Modal
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
            <Show when={copyState()}>
              <p class={ui.inlineState} role="status">{copyState()}</p>
            </Show>
          </>
        )}
      </Show>
    </Modal>
  );
}

function MemberRow(props: {
  member: OrganizationMember;
  canManage: boolean;
  isCurrentUser: boolean;
  currentUserRole: string | null | undefined;
  action: { id: string; action: "role" | "remove" } | null;
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
  const busy = (action: "role" | "remove") => props.action?.id === id() && props.action.action === action;
  return (
    <tr class={ui.detailTableRow}>
      <td>
        <div class={ui.tableCellStack}>
          <strong>{memberName(props.member)}</strong>
          <div class={ui.muted}>{props.member.user_id}</div>
        </div>
      </td>
      <td>{memberEmail(props.member)}</td>
      <td>
        <Show when={canChangeRole()} fallback={<RoleBadge role={props.member.role} />}>
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
      <td><MemberStatusBadge status={props.member.status} /></td>
      <td>{memberJoinedAt(props.member)}</td>
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
  action: { id: string; action: "revoke" } | null;
  error: string | null;
  onRevoke: (invitation: OrganizationInvitation) => void;
}) {
  const id = () => invitationResourceID(props.invitation);
  const busy = () => props.action?.id === id() && props.action.action === "revoke";
  return (
    <tr>
      <td><strong>{props.invitation.email}</strong></td>
      <td><RoleBadge role={props.invitation.role} /></td>
      <td><InvitationStatusBadge status={props.invitation.status} /></td>
      <td>{formatExpiration(props.invitation.expires_at)}</td>
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
  const [memberAction, setMemberAction] = createSignal<{ id: string; action: "role" | "remove" } | null>(null);
  const [memberError, setMemberError] = createSignal<{ id: string; message: string } | null>(null);
  const [invitationAction, setInvitationAction] = createSignal<{ id: string; action: "revoke" } | null>(null);
  const [invitationError, setInvitationError] = createSignal<{ id: string; message: string } | null>(null);

  const me = createQuery(() => ({
    queryKey: ["me"],
    queryFn: getMe,
    retry: false,
    staleTime: 60_000,
  }));
  const members = createQuery(() => ({
    queryKey: ["members"],
    queryFn: listMembers,
    retry: false,
  }));
  const invitations = createQuery(() => ({
    queryKey: ["invitations"],
    queryFn: listInvitations,
    retry: false,
  }));

  const canManage = createMemo(() => canManageFromMe(me.data?.role, me.data?.permissions));
  const pendingInvitations = createMemo(() =>
    invitations.data?.invitations ?? [],
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
    setMemberError(null);
    setMemberAction({ id, action: "role" });
    try {
      await updateMemberRole(id, role, member.role);
      await queryClient.invalidateQueries({ queryKey: ["members"] });
    } catch (error) {
      setMemberError({ id, message: membersErrorMessage(error) });
    } finally {
      setMemberAction(null);
    }
  };

  const remove = async (member: OrganizationMember) => {
    const id = memberResourceID(member);
    if (!id) return;
    if (!window.confirm(`Remove ${memberName(member)} from this organization?`)) return;
    setMemberError(null);
    setMemberAction({ id, action: "remove" });
    try {
      await removeMember(id);
      await queryClient.invalidateQueries({ queryKey: ["members"] });
    } catch (error) {
      setMemberError({ id, message: membersErrorMessage(error) });
    } finally {
      setMemberAction(null);
    }
  };

  const revoke = async (invitation: OrganizationInvitation) => {
    const id = invitationResourceID(invitation);
    if (!id) return;
    if (!window.confirm(`Revoke invitation for ${invitation.email}?`)) return;
    setInvitationError(null);
    setInvitationAction({ id, action: "revoke" });
    try {
      await revokeInvitation(id);
      await queryClient.invalidateQueries({ queryKey: ["invitations"] });
    } catch (error) {
      setInvitationError({ id, message: membersErrorMessage(error) });
    } finally {
      setInvitationAction(null);
    }
  };

  const isCurrentUser = (member: OrganizationMember) =>
    member.user_id === me.data?.user_id || memberResourceID(member) === me.data?.user_id;

  return (
    <>
      <header class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Members</h1>
          <p class={ui.pageSubtitle}>
            Organization members, roles, and pending email invitations.
          </p>
        </div>
        <Show when={canManage()}>
          <button class={ui.button} type="button" onClick={() => setModalOpen(true)}>
            Invite member
          </button>
        </Show>
      </header>

      <Show when={!me.isPending && !canManage()}>
        <p class={cx(ui.hasMoreBanner, "border-[#9bb9e8] bg-[#eef4ff] text-console-info")} role="status">
          Member settings require admin access.
        </p>
      </Show>

      <Show when={members.isError}>
        <p class={ui.error} role="alert">{membersErrorMessage(members.error)}</p>
      </Show>

      <section class={"mb-7"}>
        <div class={cx(ui.toolbar, "mb-3")}>
          <div>
            <h2 class={ui.h2}>Current members</h2>
            <p class={ui.pageSubtitle}>Active and disabled accounts currently associated with this organization.</p>
          </div>
        </div>
        <Show when={!members.isPending} fallback={<p class={ui.muted}>Loading members...</p>}>
          <Show when={(members.data?.members.length ?? 0) > 0} fallback={<p class={ui.emptyState}>No members found.</p>}>
            <div class={ui.tableWrap}>
              <table class={"min-w-245"}>
                <thead>
                  <tr>
                    <th>Member</th>
                    <th>Email</th>
                    <th>Role</th>
                    <th>Status</th>
                    <th>Joined</th>
                    <th><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  <For each={members.data?.members ?? []}>
                    {(member) => {
                      const id = memberResourceID(member);
                      return (
                        <MemberRow
                          member={member}
                          canManage={canManage()}
                          isCurrentUser={isCurrentUser(member)}
                          currentUserRole={me.data?.role}
                          action={memberAction()}
                          error={memberError()?.id === id ? memberError()?.message ?? null : null}
                          onRoleChange={changeMemberRole}
                          onRemove={remove}
                        />
                      );
                    }}
                  </For>
                </tbody>
              </table>
            </div>
          </Show>
        </Show>
      </section>

      <section>
        <div class={cx(ui.toolbar, "mb-3")}>
          <div>
            <h2 class={ui.h2}>Pending invitations</h2>
            <p class={ui.pageSubtitle}>Email invitations that have not been accepted yet.</p>
          </div>
        </div>

        <Show when={invitations.isError}>
          <p class={ui.error} role="alert">{membersErrorMessage(invitations.error)}</p>
        </Show>

        <Show when={!invitations.isPending} fallback={<p class={ui.muted}>Loading invitations...</p>}>
          <Show when={pendingInvitations().length > 0} fallback={<p class={ui.emptyState}>No pending invitations.</p>}>
            <div class={ui.tableWrap}>
              <table class={"min-w-225"}>
                <thead>
                  <tr>
                    <th>Email</th>
                    <th>Role</th>
                    <th>Status</th>
                    <th>Expires</th>
                    <th><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  <For each={pendingInvitations()}>
                    {(invitation) => {
                      const id = invitationResourceID(invitation);
                      return (
                        <InvitationRow
                          invitation={invitation}
                          canManage={canManage()}
                          action={invitationAction()}
                          error={invitationError()?.id === id ? invitationError()?.message ?? null : null}
                          onRevoke={revoke}
                        />
                      );
                    }}
                  </For>
                </tbody>
              </table>
            </div>
          </Show>
        </Show>
      </section>

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

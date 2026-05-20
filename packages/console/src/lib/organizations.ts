import { postJson } from "./api";

export type Organization = {
  id: string;
  slug: string;
  name?: string | null;
};

export type CreateOrganizationInput = {
  slug: string;
  name: string;
  setup_token?: string;
};

export async function createOrganization(input: CreateOrganizationInput): Promise<Organization> {
  return postJson<CreateOrganizationInput, Organization>("/api/organizations", input);
}

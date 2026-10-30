// The first-run setup of a deployment is one journey: the operator signs in,
// the first owner signs in, then creates the organization and its first project.
// Each of those screens shows where it sits in it.
export const SETUP_PATH = "/setup";
export const setupStep = (step: 1 | 2 | 3 | 4) => `Set up Helmr · Step ${step} of 4`;
// The first project of setup returns to the setup page, which then reports completion.
export const SETUP_PROJECT_PATH = `/projects/new?setup=1&next=${encodeURIComponent(SETUP_PATH)}`;

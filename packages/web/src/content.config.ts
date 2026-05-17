import { defineCollection } from "astro:content";
import { glob } from "astro/loaders";
import { z } from "astro/zod";

const docs = defineCollection({
  loader: glob({ base: "./src/content/docs", pattern: "**/*.md" }),
  schema: z.object({
    title: z.string(),
    description: z.string(),
    section: z.string().default("Guides"),
    sidebarLabel: z.string().optional(),
    order: z.number().int().nonnegative().default(999),
    draft: z.boolean().default(false),
  }),
});

export const collections = { docs };

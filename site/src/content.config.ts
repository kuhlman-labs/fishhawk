import { defineCollection } from 'astro:content';
import { docsLoader } from '@astrojs/starlight/loaders';
import { docsSchema } from '@astrojs/starlight/schema';

// The docs collection is rooted at site/src/content/docs/ and NOWHERE else.
// docs/ at the repository root is agent-consumed reference and is never a
// publish source for this site — the human-facing / agent-consumed split is
// structural, not a convention. Cross-links to docs/ as GitHub URLs in prose
// are expected; a loader or content root pointing at it is not.
export const collections = {
	docs: defineCollection({ loader: docsLoader(), schema: docsSchema() }),
};

// Structured data for search engines: one JSON-LD script. `<` is escaped so
// no string in the data can close the script element.
export function JsonLd({ data }: { data: object }) {
  const json = JSON.stringify(data).replace(/</g, '\\u003c')
  // biome-ignore lint/security/noDangerouslySetInnerHtml: JSON-LD must be raw text, and `<` is escaped above.
  return <script type="application/ld+json" dangerouslySetInnerHTML={{ __html: json }} />
}

// Every docs page's Open Graph image, built statically by app/og/[...slug].
export function ogImage(slugs: string[]): string {
  return `/og/${[...slugs, 'image.png'].join('/')}`
}

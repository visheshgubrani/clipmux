import { Tabs as FumaTabs, Tab, type TabsProps } from 'fumadocs-ui/components/tabs'

// Tabs with the same items share a group, so choosing "Python" once switches
// every language tab on the page, and the choice is remembered across pages.
export function Tabs({
  items,
  groupId,
  persist = true,
  ...props
}: TabsProps & { groupId?: string; persist?: boolean }) {
  return <FumaTabs items={items} groupId={groupId ?? items?.join('|')} persist={persist} {...props} />
}

export { Tab }

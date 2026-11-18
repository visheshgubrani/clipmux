// The ClipMux mark (docs/assets/logo.svg): a tally-violet square, the "mux"
// bar and a play triangle.
export function BrandMark({ size = 24 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden="true" className="flex-none">
      <rect width="32" height="32" rx="7" fill="#6d28d9" />
      <rect x="7" y="9.5" width="2.5" height="13" rx="1.25" fill="#fff" fillOpacity=".6" />
      <path d="M12.5 9.5v13l10.5-6.5z" fill="#fff" />
    </svg>
  )
}

export function Wordmark() {
  return (
    <span className="flex items-center gap-2.5 text-[17px] font-bold [font-stretch:118%] tracking-[-0.01em]">
      <BrandMark />
      ClipMux
    </span>
  )
}

// Inline stroke icons shared by the console. 16×16 grid, currentColor.
const paths = {
  logo: <><path d="M4 9h24M7 15h18M10 21h12" /><path d="M4 9l5 16h14l5-16" /></>,
  grid: <><rect x="2" y="2" width="5" height="5" rx="1" /><rect x="9" y="2" width="5" height="5" rx="1" /><rect x="2" y="9" width="5" height="5" rx="1" /><rect x="9" y="9" width="5" height="5" rx="1" /></>,
  box: <><path d="M8 1.8l5.6 3.1v6.2L8 14.2 2.4 11.1V4.9z" /><path d="M2.6 5L8 8l5.4-3M8 8v6" /></>,
  pulse: <path d="M1.5 8.5h3l2-5 3 9 2-4h3" />,
  server: <><rect x="2" y="2.5" width="12" height="4.5" rx="1" /><rect x="2" y="9" width="12" height="4.5" rx="1" /></>,
  net: <><circle cx="8" cy="3" r="1.6" /><circle cx="3" cy="13" r="1.6" /><circle cx="13" cy="13" r="1.6" /><path d="M7.2 4.4L3.8 11.6M8.8 4.4l3.4 7.2M4.6 13h6.8" /></>,
  users: <><circle cx="6" cy="5.5" r="2.5" /><path d="M1.5 13.5c.6-2.4 2.3-3.6 4.5-3.6s3.9 1.2 4.5 3.6" /><path d="M10.5 3.2a2.4 2.4 0 010 4.6M12 9.8c1.3.5 2.1 1.6 2.5 3.7" /></>,
  plus: <path d="M8 3v10M3 8h10" />,
} as const;

export type IconName = keyof typeof paths;

export function Icon({ name }: { name: IconName }) {
  const box = name === "logo" ? "0 0 32 32" : "0 0 16 16";
  return (
    <svg viewBox={box} fill="none" stroke="currentColor" strokeWidth={name === "logo" ? 2.2 : 1.5} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {paths[name]}
    </svg>
  );
}

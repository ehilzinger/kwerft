// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Inline stroke icons shared by the console. 16×16 grid, currentColor.
const paths = {
  logo: <><path d="M5 9h22l-4 11H9z" /><path d="M9 14h14" /><path d="M3 27l26-4" /></>,
  grid: <><rect x="2" y="2" width="5" height="5" rx="1" /><rect x="9" y="2" width="5" height="5" rx="1" /><rect x="2" y="9" width="5" height="5" rx="1" /><rect x="9" y="9" width="5" height="5" rx="1" /></>,
  box: <><path d="M8 1.8l5.6 3.1v6.2L8 14.2 2.4 11.1V4.9z" /><path d="M2.6 5L8 8l5.4-3M8 8v6" /></>,
  pulse: <path d="M1.5 8.5h3l2-5 3 9 2-4h3" />,
  server: <><rect x="2" y="2.5" width="12" height="4.5" rx="1" /><rect x="2" y="9" width="12" height="4.5" rx="1" /></>,
  net: <><circle cx="8" cy="3" r="1.6" /><circle cx="3" cy="13" r="1.6" /><circle cx="13" cy="13" r="1.6" /><path d="M7.2 4.4L3.8 11.6M8.8 4.4l3.4 7.2M4.6 13h6.8" /></>,
  users: <><circle cx="6" cy="5.5" r="2.5" /><path d="M1.5 13.5c.6-2.4 2.3-3.6 4.5-3.6s3.9 1.2 4.5 3.6" /><path d="M10.5 3.2a2.4 2.4 0 010 4.6M12 9.8c1.3.5 2.1 1.6 2.5 3.7" /></>,
  plus: <path d="M8 3v10M3 8h10" />,
  key: <><circle cx="5.5" cy="10.5" r="3" /><path d="M7.6 8.4L14 2M11.5 4.5l1.8 1.8M10 6l1.5 1.5" /></>,
  rocket: <><path d="M9.5 2.5c2.5-.8 4-.3 4 0s.8 1.5 0 4l-4.5 4.5-4-4z" /><path d="M5 7L2.5 7.5 4 5.5h3M9 11l-.5 2.5 2-1.5V9" /></>,
  shield: <path d="M8 1.5l5.5 2v4.2c0 3.3-2.3 5.6-5.5 6.8-3.2-1.2-5.5-3.5-5.5-6.8V3.5z" />,
  alert: <><path d="M8 2l6.5 11.5h-13z" /><path d="M8 6.5v3.2M8 11.8h.01" /></>,
  restart: <><path d="M13.5 8a5.5 5.5 0 11-1.6-3.9" /><path d="M13.5 2.5v3h-3" /></>,
  scale: <><path d="M2.5 13.5h11" /><rect x="3.5" y="8" width="2.5" height="5.5" rx=".5" /><rect x="10" y="3.5" width="2.5" height="10" rx=".5" /></>,
  trash: <><path d="M2.5 4.5h11M6 4.5V3h4v1.5M4 4.5l.7 9h6.6l.7-9" /></>,
  clock: <><circle cx="8" cy="8" r="6" /><path d="M8 4.5V8l2.5 1.5" /></>,
  disk: <><ellipse cx="8" cy="4" rx="5.5" ry="2" /><path d="M2.5 4v8c0 1.1 2.5 2 5.5 2s5.5-.9 5.5-2V4M2.5 8c0 1.1 2.5 2 5.5 2s5.5-.9 5.5-2" /></>,
  play: <path d="M5 3.2v9.6L12.6 8z" />,
  gear: <><circle cx="8" cy="8" r="2.2" /><path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M3.4 12.6l1.4-1.4M11.2 4.8l1.4-1.4" /></>,
  search: <><circle cx="7" cy="7" r="4.5" /><path d="M10.5 10.5L14 14" /></>,
  minus: <path d="M3 8h10" />,
  fit: <path d="M2 6V2h4M10 2h4v4M14 10v4h-4M6 14H2v-4" />,
  expand: <path d="M9.5 2H14v4.5M14 2L9 7M6.5 14H2V9.5M2 14l5-5" />,
  x: <path d="M4 4l8 8M12 4l-8 8" />,
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

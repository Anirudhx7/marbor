// Class strings for the shared button look. Each string is written out in
// full so Tailwind's scanner sees every class. Only the primary variant is
// used by the app so far; the others are defined here so they can be adopted
// site by site without changing how they look.

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'destructive';
export type ButtonSize = 'sm' | 'md';

const CLASSES: Record<ButtonVariant, Record<ButtonSize, string>> = {
  primary: {
    md: 'px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground text-sm font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
    sm: 'px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground text-xs font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
  },
  secondary: {
    md: 'px-4 py-2 bg-secondary hover:bg-secondary/80 text-foreground text-sm font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
    sm: 'px-4 py-2 bg-secondary hover:bg-secondary/80 text-foreground text-xs font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
  },
  ghost: {
    md: 'px-4 py-2 text-muted-foreground hover:text-foreground text-sm font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed',
    sm: 'px-4 py-2 text-muted-foreground hover:text-foreground text-xs font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed',
  },
  destructive: {
    md: 'px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground text-sm font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
    sm: 'px-4 py-2 bg-destructive hover:bg-destructive/90 text-destructive-foreground text-xs font-medium rounded-lg transition-colors shadow-sm disabled:opacity-50 disabled:cursor-not-allowed',
  },
};

export function buttonClasses(variant: ButtonVariant = 'primary', size: ButtonSize = 'md'): string {
  return CLASSES[variant][size];
}

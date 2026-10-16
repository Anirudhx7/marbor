import type { ButtonHTMLAttributes } from 'react';
import { buttonClasses, type ButtonSize, type ButtonVariant } from '../lib/buttonClasses';

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
}

// Thin wrapper over a native button: the look comes from buttonClasses and
// every other prop (handlers, disabled, type, aria) passes straight through.
// type is deliberately not defaulted, so a button keeps the type it had.
export function Button({ variant = 'primary', size = 'md', className = '', ...rest }: ButtonProps) {
  const classes = className ? `${buttonClasses(variant, size)} ${className}` : buttonClasses(variant, size);
  return <button {...rest} className={classes} />;
}

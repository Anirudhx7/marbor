import { useState, useRef, useEffect } from 'react';
import { Calendar as CalendarIcon, Clock, ChevronLeft, ChevronRight, X } from 'lucide-react';

interface CustomDateTimePickerProps {
  value: string; // YYYY-MM-DDTHH:MM
  onChange: (value: string) => void;
  placeholder?: string;
  className?: string;
  disabled?: boolean;
}

export function CustomDateTimePicker({
  value,
  onChange,
  placeholder = 'Select date & time...',
  className = '',
  disabled = false,
}: CustomDateTimePickerProps) {
  const [isOpen, setIsOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  // Parsing state
  const initialDate = value ? new Date(value) : new Date();
  const [viewYear, setViewYear] = useState(initialDate.getFullYear());
  const [viewMonth, setViewMonth] = useState(initialDate.getMonth()); // 0-11
  
  // Hour & Minute values
  const [hours, setHours] = useState(value ? new Date(value).getHours() : 0);
  const [minutes, setMinutes] = useState(value ? new Date(value).getMinutes() : 0);
  const [selectedDay, setSelectedDay] = useState<number | null>(
    value ? new Date(value).getDate() : null
  );

  // Sync state with incoming value changes
  useEffect(() => {
    if (value) {
      const d = new Date(value);
      setViewYear(d.getFullYear());
      setViewMonth(d.getMonth());
      setSelectedDay(d.getDate());
      setHours(d.getHours());
      setMinutes(d.getMinutes());
    } else {
      setSelectedDay(null);
    }
  }, [value]);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setIsOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  const months = [
    'January', 'February', 'March', 'April', 'May', 'June',
    'July', 'August', 'September', 'October', 'November', 'December'
  ];

  const daysOfWeek = ['Su', 'Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa'];

  // Helper calendar calculations
  const getDaysInMonth = (y: number, m: number) => new Date(y, m + 1, 0).getDate();
  const getFirstDayOfMonth = (y: number, m: number) => new Date(y, m, 1).getDay();

  const handlePrevMonth = () => {
    if (viewMonth === 0) {
      setViewMonth(11);
      setViewYear(viewYear - 1);
    } else {
      setViewMonth(viewMonth - 1);
    }
  };

  const handleNextMonth = () => {
    if (viewMonth === 11) {
      setViewMonth(0);
      setViewYear(viewYear + 1);
    } else {
      setViewMonth(viewMonth + 1);
    }
  };

  const selectDay = (day: number) => {
    setSelectedDay(day);
    updateValue(day, hours, minutes);
  };

  const handleHourChange = (h: number) => {
    const val = Math.max(0, Math.min(23, h));
    setHours(val);
    if (selectedDay !== null) {
      updateValue(selectedDay, val, minutes);
    }
  };

  const handleMinuteChange = (m: number) => {
    const val = Math.max(0, Math.min(59, m));
    setMinutes(val);
    if (selectedDay !== null) {
      updateValue(selectedDay, hours, val);
    }
  };

  const updateValue = (day: number, h: number, m: number) => {
    const date = new Date(viewYear, viewMonth, day, h, m);
    // Format YYYY-MM-DDTHH:MM
    const yyyy = date.getFullYear();
    const mm = String(date.getMonth() + 1).padStart(2, '0');
    const dd = String(date.getDate()).padStart(2, '0');
    const hh = String(date.getHours()).padStart(2, '0');
    const min = String(date.getMinutes()).padStart(2, '0');
    onChange(`${yyyy}-${mm}-${dd}T${hh}:${min}`);
  };

  const handleClear = () => {
    onChange('');
    setSelectedDay(null);
    setIsOpen(false);
  };

  // Generate calendar days grid
  const daysInMonth = getDaysInMonth(viewYear, viewMonth);
  const firstDayIndex = getFirstDayOfMonth(viewYear, viewMonth);

  const prevMonthDaysCount = getDaysInMonth(
    viewMonth === 0 ? viewYear - 1 : viewYear,
    viewMonth === 0 ? 11 : viewMonth - 1
  );

  const calendarGrid = [];

  // Previous month padding days
  for (let i = firstDayIndex - 1; i >= 0; i--) {
    calendarGrid.push({
      day: prevMonthDaysCount - i,
      isCurrentMonth: false,
      monthOffset: -1
    });
  }

  // Current month days
  for (let i = 1; i <= daysInMonth; i++) {
    calendarGrid.push({
      day: i,
      isCurrentMonth: true,
      monthOffset: 0
    });
  }

  // Next month padding days
  const remainingCells = 42 - calendarGrid.length;
  for (let i = 1; i <= remainingCells; i++) {
    calendarGrid.push({
      day: i,
      isCurrentMonth: false,
      monthOffset: 1
    });
  }

  // Format display text
  const formatDisplay = () => {
    if (!value) return '';
    try {
      const d = new Date(value);
      if (isNaN(d.getTime())) return value;
      return d.toLocaleString(undefined, {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
        hour12: false,
      });
    } catch {
      return value;
    }
  };

  return (
    <div ref={containerRef} className={`relative min-w-0 w-full ${className}`}>
      <div className="relative">
        <button
          type="button"
          disabled={disabled}
          onClick={() => setIsOpen(!isOpen)}
          className={`flex items-center justify-between w-full border border-border bg-secondary/30 text-foreground px-3 py-2 text-sm rounded-lg text-left transition-all ${
            disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer hover:bg-secondary/50'
          }`}
        >
          <span className={`truncate ${!value ? 'text-muted-foreground/60' : ''}`}>
            {value ? formatDisplay() : placeholder}
          </span>
          <CalendarIcon className="w-4 h-4 ml-2 shrink-0 text-primary" />
        </button>

        {!!value && !disabled && (
          <button
            type="button"
            onClick={handleClear}
            className="absolute right-8 top-1/2 -translate-y-1/2 p-0.5 rounded text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
          >
            <X className="w-3.5 h-3.5" />
          </button>
        )}
      </div>

      {isOpen && (
        <div className="absolute z-50 mt-1.5 p-4 border border-border bg-card rounded-xl shadow-xl w-80 animate-fade-in">
          {/* Calendar header */}
          <div className="flex items-center justify-between mb-3">
            <span className="font-semibold text-sm text-foreground">
              {months[viewMonth]} {viewYear}
            </span>
            <div className="flex items-center gap-1">
              <button
                type="button"
                onClick={handlePrevMonth}
                className="p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
              >
                <ChevronLeft className="w-4 h-4" />
              </button>
              <button
                type="button"
                onClick={handleNextMonth}
                className="p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
              >
                <ChevronRight className="w-4 h-4" />
              </button>
            </div>
          </div>

          {/* Days of week header */}
          <div className="grid grid-cols-7 gap-1 text-center text-xs font-semibold text-muted-foreground/75 mb-2">
            {daysOfWeek.map((d) => (
              <div key={d} className="py-0.5">{d}</div>
            ))}
          </div>

          {/* Calendar days grid */}
          <div className="grid grid-cols-7 gap-1 mb-4">
            {calendarGrid.map((cell, idx) => {
              const isSelected =
                cell.isCurrentMonth && selectedDay === cell.day;
              
              const isToday =
                cell.isCurrentMonth &&
                new Date().getDate() === cell.day &&
                new Date().getMonth() === viewMonth &&
                new Date().getFullYear() === viewYear;

              return (
                <button
                  key={idx}
                  type="button"
                  onClick={() => {
                    if (cell.isCurrentMonth) {
                      selectDay(cell.day);
                    } else {
                      // Switch month
                      const targetDate = new Date(viewYear, viewMonth + cell.monthOffset, cell.day);
                      setViewYear(targetDate.getFullYear());
                      setViewMonth(targetDate.getMonth());
                      setSelectedDay(targetDate.getDate());
                      updateValue(targetDate.getDate(), hours, minutes);
                    }
                  }}
                  className={`text-xs py-1.5 rounded-lg text-center font-medium transition-colors ${
                    isSelected
                      ? 'bg-primary text-primary-foreground font-semibold shadow-sm'
                      : cell.isCurrentMonth
                      ? 'text-foreground hover:bg-secondary/60'
                      : 'text-muted-foreground/40 hover:bg-secondary/40'
                  } ${isToday && !isSelected ? 'border border-primary/50' : ''}`}
                >
                  {cell.day}
                </button>
              );
            })}
          </div>

          {/* Time Picker Row */}
          <div className="border-t border-border/80 pt-3 flex items-center justify-between">
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Clock className="w-3.5 h-3.5 text-primary shrink-0" />
              <span className="font-semibold">Time:</span>
            </div>

            <div className="flex items-center gap-1 font-mono text-sm">
              <input
                type="number"
                min="0"
                max="23"
                value={String(hours).padStart(2, '0')}
                onChange={(e) => handleHourChange(Number(e.target.value))}
                className="w-10 px-1.5 py-1 text-center bg-secondary/80 border border-border rounded-md text-foreground focus:outline-none focus:ring-1 focus:ring-primary"
              />
              <span className="text-muted-foreground font-semibold">:</span>
              <input
                type="number"
                min="0"
                max="59"
                value={String(minutes).padStart(2, '0')}
                onChange={(e) => handleMinuteChange(Number(e.target.value))}
                className="w-10 px-1.5 py-1 text-center bg-secondary/80 border border-border rounded-md text-foreground focus:outline-none focus:ring-1 focus:ring-primary"
              />
            </div>
          </div>

          {/* Action buttons */}
          <div className="flex justify-end gap-2 mt-4 pt-3 border-t border-border/80 text-xs">
            <button
              type="button"
              onClick={handleClear}
              className="px-2.5 py-1.5 text-muted-foreground hover:text-foreground transition-colors"
            >
              Clear
            </button>
            <button
              type="button"
              onClick={() => setIsOpen(false)}
              className="px-3 py-1.5 bg-primary text-primary-foreground font-medium rounded-lg hover:bg-primary/95 transition-colors"
            >
              Done
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

interface CustomTimePickerProps {
  value: string; // HH:MM
  onChange: (value: string) => void;
  placeholder?: string;
  className?: string;
  disabled?: boolean;
}

export function CustomTimePicker({
  value,
  onChange,
  placeholder = 'Select time...',
  className = '',
  disabled = false,
}: CustomTimePickerProps) {
  const [isOpen, setIsOpen] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  const [hours, setHours] = useState(value ? parseInt(value.split(':')[0], 10) : 0);
  const [minutes, setMinutes] = useState(value ? parseInt(value.split(':')[1], 10) : 0);

  useEffect(() => {
    if (value && value.includes(':')) {
      const parts = value.split(':');
      setHours(parseInt(parts[0], 10));
      setMinutes(parseInt(parts[1], 10));
    }
  }, [value]);

  useEffect(() => {
    function handleClickOutside(event: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(event.target as Node)) {
        setIsOpen(false);
      }
    }
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, []);

  const handleHourChange = (h: number) => {
    const val = Math.max(0, Math.min(23, h));
    setHours(val);
    updateValue(val, minutes);
  };

  const handleMinuteChange = (m: number) => {
    const val = Math.max(0, Math.min(59, m));
    setMinutes(val);
    updateValue(hours, val);
  };

  const updateValue = (h: number, m: number) => {
    const hh = String(h).padStart(2, '0');
    const mm = String(m).padStart(2, '0');
    onChange(`${hh}:${mm}`);
  };

  // Predefined quick select values
  const hourOptions = Array.from({ length: 24 }, (_, i) => i);
  const minuteOptions = [0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55];

  return (
    <div ref={containerRef} className={`relative min-w-0 w-full ${className}`}>
      <div className="relative">
        <button
          type="button"
          disabled={disabled}
          onClick={() => setIsOpen(!isOpen)}
          className={`flex items-center justify-between w-full border border-border bg-secondary/30 text-foreground px-3 py-2 text-sm rounded-lg text-left transition-all ${
            disabled ? 'opacity-50 cursor-not-allowed' : 'cursor-pointer hover:bg-secondary/50'
          }`}
        >
          <span className={`truncate font-mono ${!value ? 'text-muted-foreground/60' : ''}`}>
            {value ? value : placeholder}
          </span>
          <Clock className="w-4 h-4 ml-2 shrink-0 text-primary" />
        </button>

        {!!value && !disabled && (
          <button
            type="button"
            onClick={() => onChange('')}
            className="absolute right-8 top-1/2 -translate-y-1/2 p-0.5 rounded text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
          >
            <X className="w-3.5 h-3.5" />
          </button>
        )}
      </div>

      {isOpen && (
        <div className="absolute z-50 mt-1.5 p-4 border border-border bg-card rounded-xl shadow-xl w-64 animate-fade-in">
          {/* Scrollable selectors */}
          <div className="flex gap-4 mb-4">
            {/* Hours column */}
            <div className="flex-1">
              <span className="block text-[10px] uppercase font-bold text-muted-foreground/60 mb-1.5 text-center">Hours</span>
              <div className="h-40 overflow-y-auto border border-border/80 rounded-lg bg-secondary/20 py-1 scrollbar-thin">
                {hourOptions.map((h) => {
                  const isSelected = h === hours;
                  return (
                    <button
                      key={h}
                      type="button"
                      onClick={() => handleHourChange(h)}
                      className={`block w-full py-1 text-center text-xs font-mono transition-colors hover:bg-primary/10 hover:text-primary ${
                        isSelected ? 'bg-primary/15 text-primary font-bold' : 'text-foreground'
                      }`}
                    >
                      {String(h).padStart(2, '0')}
                    </button>
                  );
                })}
              </div>
            </div>

            {/* Minutes column */}
            <div className="flex-1">
              <span className="block text-[10px] uppercase font-bold text-muted-foreground/60 mb-1.5 text-center">Minutes</span>
              <div className="h-40 overflow-y-auto border border-border/80 rounded-lg bg-secondary/20 py-1 scrollbar-thin">
                {minuteOptions.map((m) => {
                  const isSelected = m === minutes;
                  return (
                    <button
                      key={m}
                      type="button"
                      onClick={() => handleMinuteChange(m)}
                      className={`block w-full py-1 text-center text-xs font-mono transition-colors hover:bg-primary/10 hover:text-primary ${
                        isSelected ? 'bg-primary/15 text-primary font-bold' : 'text-foreground'
                      }`}
                    >
                      {String(m).padStart(2, '0')}
                    </button>
                  );
                })}
              </div>
            </div>
          </div>

          {/* Custom fine-tune controls */}
          <div className="border-t border-border/80 pt-3 flex items-center justify-between text-xs text-muted-foreground">
            <span>Manual adjustment:</span>
            <div className="flex items-center gap-1 font-mono">
              <input
                type="number"
                min="0"
                max="23"
                value={String(hours).padStart(2, '0')}
                onChange={(e) => handleHourChange(Number(e.target.value))}
                className="w-10 px-1 py-0.5 text-center bg-secondary border border-border rounded-md text-foreground focus:outline-none focus:ring-1 focus:ring-primary"
              />
              <span>:</span>
              <input
                type="number"
                min="0"
                max="59"
                value={String(minutes).padStart(2, '0')}
                onChange={(e) => handleMinuteChange(Number(e.target.value))}
                className="w-10 px-1 py-0.5 text-center bg-secondary border border-border rounded-md text-foreground focus:outline-none focus:ring-1 focus:ring-primary"
              />
            </div>
          </div>

          {/* Footer Action buttons */}
          <div className="flex justify-end gap-2 mt-4 pt-3 border-t border-border/80 text-xs">
            <button
              type="button"
              onClick={() => {
                onChange('');
                setIsOpen(false);
              }}
              className="px-2.5 py-1.5 text-muted-foreground hover:text-foreground transition-colors"
            >
              Clear
            </button>
            <button
              type="button"
              onClick={() => setIsOpen(false)}
              className="px-3 py-1.5 bg-primary text-primary-foreground font-medium rounded-lg hover:bg-primary/95 transition-colors"
            >
              Done
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

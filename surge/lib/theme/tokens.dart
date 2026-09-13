import 'package:flutter/material.dart';

// KiroaaS visual language, extracted from its Tauri/React source:
// dark shell + light content, lime accent, 32px card radius, pill buttons.
abstract class Tokens {
  static const shell = Color(0xFF111111);
  static const navActive = Color(0xFF2A2A2A);
  static const navHover = Color(0xFF1A1A1A);
  static const contentBg = Color(0xFFF3F3F2);
  static const cardBg = Color(0xFFFFFFFF);
  static const accent = Color(0xFFEBFD93);
  static const accentText = Color(0xFF3F6212);
  static const statusDot = Color(0xFFA3E635);
  static const textMuted = Color(0xFF78716C);
  static const logBg = Color(0xFF252526);
  static const warn = Color(0xFFB45309);
  static const warnBg = Color(0xFFFFFBEB);
  static const err = Color(0xFFEF4444);
  static const errBg = Color(0xFFFEF2F2);

  static const cardRadius = 32.0;
  static const navRadius = 16.0;
  static const iconRadius = 12.0;
  static const logRadius = 20.0;

  static ThemeData theme() {
    return ThemeData(
      useMaterial3: true,
      scaffoldBackgroundColor: shell,
      colorScheme: ColorScheme.fromSeed(
        seedColor: accent,
        brightness: Brightness.light,
      ).copyWith(primaryContainer: accent, surface: contentBg),
      cardTheme: CardThemeData(
        color: cardBg,
        elevation: 1,
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(cardRadius),
        ),
      ),
      textTheme: const TextTheme(
        displaySmall: TextStyle(fontWeight: FontWeight.bold, letterSpacing: -1.5),
        headlineSmall: TextStyle(fontWeight: FontWeight.bold, letterSpacing: -0.5),
        labelSmall: TextStyle(fontSize: 10, fontWeight: FontWeight.w600, letterSpacing: 1.2),
      ),
    );
  }
}

class PillButton extends StatelessWidget {
  final String label;
  final VoidCallback? onPressed;
  final Color? color;
  const PillButton(this.label, {super.key, this.onPressed, this.color});

  @override
  Widget build(BuildContext context) {
    return SizedBox(
      height: 48,
      child: FilledButton(
        style: FilledButton.styleFrom(
          backgroundColor: color ?? Tokens.accent,
          foregroundColor: Tokens.accentText,
          padding: const EdgeInsets.symmetric(horizontal: 24),
          shape: const StadiumBorder(),
        ),
        onPressed: onPressed,
        child: Text(label, style: const TextStyle(fontWeight: FontWeight.bold)),
      ),
    );
  }
}

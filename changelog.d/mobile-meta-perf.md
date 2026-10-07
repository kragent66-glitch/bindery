### Added
- Bindery can be added to a phone or desktop home screen as an app: it now has a web app manifest, an Apple touch icon and a browser toolbar colour that follows the light or dark theme, including when you override the system theme in Bindery. Installed, it keeps the header and bottom bars clear of the notch and home indicator, and it works under a URL base (#3052).

### Changed
- Pages load faster, especially on phones: languages other than English are now downloaded only when selected, which cuts the main script by about a third, and cover and author images in lists and grids load as they scroll into view (#3052).

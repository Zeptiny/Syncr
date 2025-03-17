from django import template

register = template.Library()

@register.filter
def divide(value, divisor):
    try:
        return float(value) / float(divisor)
    except (ValueError, ZeroDivisionError):
        return None

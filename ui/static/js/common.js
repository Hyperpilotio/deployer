var $deployments = $('.deployment-list')
var $filterUser = $('.filterUser')
$(function() {
    if ($deployments) {
        fixDeploymentHeight();
        $('.deployment-list li').click(function() {
            $('.deployment-list li').removeClass('active');
            $(this).addClass('active');
            $('main h1').remove();
        });
    }

    if ($filterUser) {
        $('.dropdown-menu a').click(function() {
            $('#userId').val($(this).data('value'));
            $('#filterUserId').val($(this).data('value'));
            $('#statusMsg').show();
            $('#statusMsg').html('querying...');
            $('.nav-tabs li.active a').click();
        });

        $('#filterUserId').on('keyup blur change', function(e) {
            $('#userId').val($(this).val());
            $('#statusMsg').show();
            $('#statusMsg').html('querying...');
            $('.nav-tabs li.active a').click();
        });
    }
});

function fixDeploymentHeight() {
    var realHeight = $deployments.height();
    $deployments.css('height', (realHeight + 100) + 'px');
}